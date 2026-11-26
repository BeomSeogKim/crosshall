package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

const linkEventPollInterval = 250 * time.Millisecond

type servedLinkEvents struct {
	peer         domain.LinkEventPeer
	allowWake    bool
	allowPolicy  bool
	boot         string
	sent         uint64
	catchThrough uint64
	caughtUp     bool
	more         bool
	snapshotSeq  uint64
	snapshotPart int
	ackSeq       uint64
	ackMore      bool
}

type linkFrameRead struct {
	frame link.Frame
	err   error
}

func readLinkEventFrames(ctx context.Context, r *link.Reader, frames chan<- linkFrameRead) {
	for {
		frame, err := r.Read()
		select {
		case frames <- linkFrameRead{frame: frame, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func serveLinkEvents(ctx context.Context, env Env, stdin io.Reader, r *link.Reader, w *link.Writer, session link.Session, peer domain.LinkEventPeer, o outputs) core.ExitCode {
	ctx, cancel := context.WithCancel(ctx)
	stream := servedLinkEvents{peer: peer, allowWake: slices.Contains(session.Caps, link.CapWake),
		allowPolicy: slices.Contains(session.Caps, link.CapPolicy)}
	defer func() {
		cancel()
		if c, ok := stdin.(io.Closer); ok {
			_ = c.Close()
		}
		if stream.boot != "" {
			endCtx, end := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer end()
			closeEnv := env
			closeEnv.Start = nil
			_, _ = ownerData[domain.LinkEventsResult](endCtx, closeEnv,
				Call{Command: domain.LinkEvents{LinkEventPeer: peer, Close: true}})
		}
	}()
	frames := make(chan linkFrameRead, 1)
	go readLinkEventFrames(ctx, r, frames)
	ticker := time.NewTicker(linkEventPollInterval)
	defer ticker.Stop()
	for {
		var received linkFrameRead
		poll := false
		if stream.more {
			select {
			case <-ctx.Done():
				return o.fail(core.ExitError, CodeLocalError, ctx.Err().Error(), "")
			case received = <-frames:
			default:
				poll = true
			}
		} else {
			select {
			case <-ctx.Done():
				return o.fail(core.ExitError, CodeLocalError, ctx.Err().Error(), "")
			case received = <-frames:
			case <-ticker.C:
				if stream.boot == "" {
					continue
				}
				poll = true
			}
		}
		if !poll {
			if errors.Is(received.err, io.EOF) {
				return core.ExitOK
			}
			if received.err != nil {
				return o.fail(core.ExitError, CodeLocalError, received.err.Error(), "")
			}
			handled, err := stream.handleFrame(ctx, env, w, received.frame)
			if err != nil {
				return failLinkEventServe(o, err)
			}
			if !handled {
				if code := serveLinkCommand(ctx, env, w, session, peer, received.frame, o); code != core.ExitOK {
					return code
				}
			}
			if !stream.more {
				continue
			}
		}
		if err := stream.fetch(ctx, env, w); err != nil {
			return failLinkEventServe(o, err)
		}
	}
}

func failLinkEventServe(o outputs, err error) core.ExitCode {
	var refusal linkCommandRefusal
	if errors.As(err, &refusal) && refusal.code == domain.CodeLinkStale {
		return failStaleLink(o)
	}
	return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
}

func (s *servedLinkEvents) handleFrame(ctx context.Context, env Env, w *link.Writer, f link.Frame) (bool, error) {
	switch f.Type {
	case link.TypeSubscribe:
		var in link.Subscribe
		if err := json.Unmarshal(f.Body, &in); err != nil || in.Boot == "" {
			return true, errors.New("link: malformed subscribe")
		}
		return true, s.subscribe(ctx, env, w, in)
	case link.TypeAck:
		var in link.Ack
		if err := json.Unmarshal(f.Body, &in); err != nil {
			return true, errors.New("link: malformed ack")
		}
		return true, s.ack(ctx, env, in)
	case link.TypeFact:
		var in link.Fact
		if err := json.Unmarshal(f.Body, &in); err != nil {
			return true, errors.New("link: malformed fact")
		}
		return true, writeLinkRecorded(ctx, env, w, domain.LinkFact{LinkEventPeer: s.peer, Fact: in}, in.ID)
	case link.TypeWakeReady, link.TypeWakeResult:
		if !s.allowWake {
			return false, nil
		}
		return true, s.handleWakeFrame(ctx, env, w, f)
	case link.TypeWritten:
		var in link.Written
		if err := json.Unmarshal(f.Body, &in); err != nil {
			return true, errors.New("link: malformed written")
		}
		return true, writeLinkRecorded(ctx, env, w, domain.LinkWritten{LinkEventPeer: s.peer, Written: in}, in.ID)
	default:
		return false, nil
	}
}

func (s *servedLinkEvents) subscribe(ctx context.Context, env Env, w *link.Writer, in link.Subscribe) error {
	if s.ackMore || s.snapshotPart != 0 {
		return errors.New("link: subscribe interrupted a multipart transfer")
	}
	page, err := ownerData[domain.LinkEventsResult](ctx, env, Call{Command: domain.LinkEvents{LinkEventPeer: s.peer, Subscribe: &in, Resubscribe: s.boot != "", AllowWake: s.allowWake,
		AllowPolicy: s.allowPolicy}})
	if err != nil {
		return err
	}
	s.boot = in.Boot
	s.sent = in.FromSeq
	s.catchThrough = page.Through
	s.caughtUp = false
	s.ackSeq = 0
	if page.Snapshot != nil {
		s.sent = 0
		return s.writeSnapshot(w, page)
	}
	return s.writeEvents(w, page)
}

func (s *servedLinkEvents) fetch(ctx context.Context, env Env, w *link.Writer) error {
	cmd := domain.LinkEvents{LinkEventPeer: s.peer}
	if s.snapshotPart != 0 {
		part := s.snapshotPart
		cmd.SnapshotPart = &part
	} else {
		cmd.FromSeq = s.sent
	}
	page, err := ownerData[domain.LinkEventsResult](ctx, env, Call{Command: cmd})
	if err != nil {
		return err
	}
	if s.snapshotPart != 0 {
		return s.writeSnapshot(w, page)
	}
	return s.writeEvents(w, page)
}

func (s *servedLinkEvents) writeSnapshot(w *link.Writer, page domain.LinkEventsResult) error {
	part := page.Snapshot
	if part == nil || len(page.Events) != 0 || part.Part != s.snapshotPart || part.Seq > page.Through ||
		(s.snapshotPart != 0 && part.Seq != s.snapshotSeq) {
		return errors.New("link: invalid snapshot page")
	}
	if err := w.Write(link.TypeSnapshot, part); err != nil {
		return err
	}
	s.snapshotSeq = part.Seq
	if !part.Last {
		s.snapshotPart++
		s.more = true
		return nil
	}
	s.sent = part.Seq
	s.snapshotPart = 0
	s.more = s.sent < page.Through
	return s.writeCaughtUp(w)
}

func (s *servedLinkEvents) writeEvents(w *link.Writer, page domain.LinkEventsResult) error {
	if page.Snapshot != nil || page.Through < s.sent || (page.Through > s.sent && len(page.Events) == 0) {
		return errors.New("link: invalid event page")
	}
	if err := s.writeCaughtUp(w); err != nil {
		return err
	}
	for _, event := range page.Events {
		if event.Seq <= s.sent || event.Seq != s.sent+1 || event.Seq > page.Through {
			return errors.New("link: event sequence is not contiguous")
		}
		if err := w.Write(link.TypeEvent, event); err != nil {
			return err
		}
		s.sent = event.Seq
		if err := s.writeCaughtUp(w); err != nil {
			return err
		}
	}
	s.more = s.sent < page.Through
	return nil
}

func (s *servedLinkEvents) writeCaughtUp(w *link.Writer) error {
	if s.caughtUp || s.sent < s.catchThrough {
		return nil
	}
	if err := w.Write(link.TypeCaughtUp, link.CaughtUp{Seq: s.sent}); err != nil {
		return err
	}
	s.caughtUp = true
	return nil
}

func (s *servedLinkEvents) ack(ctx context.Context, env Env, in link.Ack) error {
	if s.boot == "" || in.Boot != s.boot || s.snapshotPart != 0 || in.Seq > s.sent || in.Seq < s.ackSeq ||
		(s.ackMore && in.Seq != s.ackSeq) {
		return errors.New("link: acknowledgement does not match the subscribed stream")
	}
	parts, err := splitLinkAck(domain.LinkAck{LinkEventPeer: s.peer, Ack: in})
	if err != nil {
		return err
	}
	for _, part := range parts {
		result, err := ownerCall(ctx, env, Call{Command: part})
		if err != nil {
			return err
		}
		if result.Status != core.ExitOK {
			return linkOwnerRefusal(part, result)
		}
	}
	s.ackSeq, s.ackMore = in.Seq, in.More
	return nil
}

func splitLinkAck(in domain.LinkAck) ([]domain.LinkAck, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	// Leave space for the owner spool's wire request around the command.
	if len(data) <= link.MaxFrame-1024 {
		return []domain.LinkAck{in}, nil
	}
	if len(in.Ack.Cuts) < 2 {
		return nil, errors.New("link: acknowledgement cut exceeds the owner request limit")
	}
	first, last := in, in
	mid := len(in.Ack.Cuts) / 2
	first.Ack.Cuts, first.Ack.More = in.Ack.Cuts[:mid], true
	last.Ack.Cuts = in.Ack.Cuts[mid:]
	left, err := splitLinkAck(first)
	if err != nil {
		return nil, err
	}
	right, err := splitLinkAck(last)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

func writeLinkRecorded(ctx context.Context, env Env, w *link.Writer, cmd domain.Command, id uint64) error {
	recorded, err := ownerData[link.Recorded](ctx, env, Call{Command: cmd})
	if err != nil {
		return err
	}
	if recorded.ID != id {
		return errors.New("link: recorded result did not match its request")
	}
	return w.Write(link.TypeRecorded, recorded)
}
