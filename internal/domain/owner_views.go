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
	Channels      []OwnerWhoChannel   `json:"channels"`
	StoppedBuilds []OwnerStoppedBuild `json:"stopped_builds,omitempty"`
}

type OwnerStoppedBuild struct {
	Adapter         string `json:"adapter"`
	Version         string `json:"version"`
	Platform        string `json:"platform"`
	Mode            string `json:"mode"`
	StoppedAtMS     int64  `json:"stopped_at_ms"`
	Notice          int64  `json:"notice"`
	Reason          string `json:"reason"`
	ResumeBindingID int64  `json:"resume_binding_id"`
	OriginAlias     string `json:"origin_alias,omitempty"`
}

type OwnerWhoChannel struct {
	Name    string        `json:"name"`
	Topic   string        `json:"topic,omitempty"`
	Members []OwnerMember `json:"members"`
}

type OwnerMember struct {
	Member
	BindingState  string         `json:"binding_state"`
	Reach         string         `json:"reach,omitempty"`
	LastEvidence  *OwnerEvidence `json:"last_evidence"`
	EvidenceStale bool           `json:"evidence_stale"`
	ExitTracking  ExitTracking   `json:"exit_tracking,omitempty"`
	HostAlive     bool           `json:"host_alive,omitempty"`
	OpenRequests  []int64        `json:"open_request_ids"`
	DepartToken   string         `json:"depart_token,omitempty"`
}

type OwnerEvidence struct {
	Fact   string `json:"fact"`
	Source string `json:"source"`
	AtMS   int64  `json:"at_ms"`
	AgeS   int64  `json:"age_s"`
}

func (s *Service) ownerWho(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "the owner can list bindings across channels", "crosshall who"), nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id, c.name, c.topic, b.id, b.principal_id, b.alias, b.state, b.focus, p.vendor,
		b.joined_at_ms, cmd.observed_at_ms, cmd.source, ap.adapter, json_extract(l.profile, '$.manifest_rev')
		FROM channel c LEFT JOIN binding b ON b.channel_id=c.id LEFT JOIN principal p ON p.id=b.principal_id
		LEFT JOIN observation cmd ON cmd.seq=(SELECT o.seq FROM observation o
			WHERE o.principal_id=b.principal_id AND o.kind='command' ORDER BY o.observed_at_ms DESC, o.seq DESC LIMIT 1)
		LEFT JOIN `+currentBindingLaunch+`
		LEFT JOIN adapter_policy ap ON ap.adapter = json_extract(l.profile, '$.adapter') AND ap.enabled = 1
			AND l.fenced = 0 AND b.dnd = 0 AND (b.owner_hook_fence_ms IS NULL OR l.started_hook_at_ms > b.owner_hook_fence_ms)
			AND b.id = (SELECT MIN(id) FROM binding WHERE principal_id = b.principal_id)
		ORDER BY c.id, b.id`)
	if err != nil {
		return core.Result{}, err
	}
	defer rows.Close()
	out := OwnerWhoResult{Channels: []OwnerWhoChannel{}}
	nowMS := ms(now)
	const commandCap = 30 * time.Minute
	var previous int64
	for rows.Next() {
		var channel int64
		var name, topic string
		var binding, principal, joined, commandAt sql.NullInt64
		var alias, bindingState, focus, vendor, commandSource, promptAdapter, manifestRev sql.NullString
		if err := rows.Scan(&channel, &name, &topic, &binding, &principal, &alias, &bindingState, &focus, &vendor,
			&joined, &commandAt, &commandSource, &promptAdapter, &manifestRev); err != nil {
			return core.Result{}, err
		}
		if len(out.Channels) == 0 || channel != previous {
			out.Channels = append(out.Channels, OwnerWhoChannel{Name: name, Topic: topic, Members: []OwnerMember{}})
			previous = channel
		}
		if binding.Valid {
			seenAt := joined.Int64
			if commandAt.Valid {
				seenAt = commandAt.Int64
			}
			member := OwnerMember{
				Member: Member{
					Alias: alias.String, Focus: focus.String, Vendor: vendor.String, BindingID: binding.Int64,
					State: "unknown", LastCommandAgoS: max((nowMS-seenAt)/1000, 0), Route: "pull",
					principal: principal.Int64,
				},
				BindingState:  bindingState.String,
				EvidenceStale: nowMS-seenAt >= commandCap.Milliseconds(),
			}
			if commandAt.Valid {
				member.LastEvidence = &OwnerEvidence{Fact: "command", Source: commandSource.String,
					AtMS: commandAt.Int64, AgeS: member.LastCommandAgoS}
			}
			if member.BindingState == "departed" {
				member.Route = "unavailable"
			} else {
				member.Reach = "pull"
				if promptAdapter.Valid && manifestRev.Valid && s.HasPromptLine != nil && s.HasPromptLine(promptAdapter.String, manifestRev.String) {
					member.Reach = "prompt"
				}
			}
			ch := &out.Channels[len(out.Channels)-1]
			ch.Members = append(ch.Members, member)
		}
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, err
	}
	rows.Close()
	for i := range out.Channels {
		for j := range out.Channels[i].Members {
			m := &out.Channels[i].Members[j]
			publish, err := publishStatusOf(ctx, tx, m.BindingID, now)
			if err != nil {
				return core.Result{}, err
			}
			m.Publish = publish
			if m.BindingState == "live" && publish != nil && PublishReaches(*publish) {
				m.Reach = "notice"
			}
			fact, err := latestFact(ctx, tx, m.principal)
			if err != nil {
				return core.Result{}, err
			}
			if fact != nil {
				observed := fact.At(nowMS)
				m.Observed = &observed
				if fact.Verified && (m.LastEvidence == nil || fact.AtMS > m.LastEvidence.AtMS) {
					m.LastEvidence = &OwnerEvidence{Fact: fact.Fact, Source: fact.Source, AtMS: fact.AtMS, AgeS: observed.AgeS}
					m.EvidenceStale = !observed.Current
				}
			}
			m.OpenRequests, err = ownerOpenRequests(ctx, tx, m.BindingID)
			if err != nil {
				return core.Result{}, err
			}
			if m.BindingState == "live" {
				tracking, host, err := s.exitTracking(ctx, tx, m.principal)
				if err != nil {
					return core.Result{}, err
				}
				m.ExitTracking = tracking
				if tracking != ExitUntracked && s.ProcessAlive != nil && s.judgeableHost(&host) && s.ProcessAlive(host.PID, host.Start) {
					m.HostAlive = true
					m.EvidenceStale = false
				}
				m.DepartToken, err = departureToken(ctx, tx, m.BindingID, m.principal)
				if err != nil {
					return core.Result{}, err
				}
			}
		}
	}
	out.StoppedBuilds, err = ownerStoppedBuilds(ctx, tx)
	if err != nil {
		return core.Result{}, err
	}
	return OK(out)
}

func ownerStoppedBuilds(ctx context.Context, tx store.Tx) ([]OwnerStoppedBuild, error) {
	rows, err := tx.QueryContext(ctx, `SELECT cb.vendor, cb.vendor_version, cb.platform, cb.mode, cb.stopped_at_ms,
		cb.stop_ref, cb.stop_reason, COALESCE(MAX(pa.binding_id), MIN(b.id)),
		COALESCE(MAX(CASE WHEN pa.id IS NOT NULL THEN b.alias END), '') FROM binding b JOIN `+currentBindingLaunch+`
		JOIN capability_build cb ON `+resumableBindingBuild+`
		LEFT JOIN publish_attempt pa ON pa.id = cb.stop_ref AND pa.binding_id = b.id
		WHERE cb.cap = 'publish' AND `+store.CapabilityBuildStopped("cb")+`
		GROUP BY cb.vendor, cb.vendor_version, cb.platform, cb.mode, cb.manifest_rev
		ORDER BY cb.vendor, cb.vendor_version, cb.platform, cb.mode, cb.manifest_rev`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var builds []OwnerStoppedBuild
	for rows.Next() {
		var b OwnerStoppedBuild
		if err := rows.Scan(&b.Adapter, &b.Version, &b.Platform, &b.Mode, &b.StoppedAtMS,
			&b.Notice, &b.Reason, &b.ResumeBindingID, &b.OriginAlias); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

func ownerOpenRequests(ctx context.Context, tx store.Tx, binding int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.message_id FROM binding b
		JOIN obligation o ON o.principal_id=b.principal_id JOIN message m ON m.id=o.message_id
		WHERE b.id=? AND m.channel_id=b.channel_id AND o.state='open' ORDER BY o.message_id`, binding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
