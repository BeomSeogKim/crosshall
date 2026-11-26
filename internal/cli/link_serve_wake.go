package cli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

func (s *servedLinkEvents) handleWakeFrame(ctx context.Context, env Env, w *link.Writer, f link.Frame) error {
	if f.Type == link.TypeWakeReady {
		var route link.WakeRoute
		if err := json.Unmarshal(f.Body, &route); err != nil {
			return errors.New("link: malformed wake route")
		}
		if !s.caughtUp || s.snapshotPart != 0 || s.ackMore {
			return nil
		}
		result, err := ownerData[domain.LinkWakeReadyResult](ctx, env,
			Call{Command: domain.LinkWakeReady{LinkEventPeer: s.peer, Route: route}})
		if err != nil {
			return err
		}
		if result.Seq > s.sent {
			s.more = true
		} else if result.Replay != nil {
			return w.Write(link.TypeWakeReplay, result.Replay)
		}
		return nil
	}
	var input link.WakeResult
	if err := json.Unmarshal(f.Body, &input); err != nil {
		return errors.New("link: malformed wake result")
	}
	recorded, err := ownerData[link.WakeRecorded](ctx, env,
		Call{Command: domain.LinkWakeResult{LinkEventPeer: s.peer, Result: input}})
	if err != nil {
		return err
	}
	if recorded.AttemptID != input.AttemptID {
		return errors.New("link: wake receipt did not match its request")
	}
	return w.Write(link.TypeWakeRecorded, recorded)
}
