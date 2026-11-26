package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

type eventHomeTransport struct {
	homeTransport
	handle func(domain.Command) (core.Result, error)
}

func (h *eventHomeTransport) Do(ctx context.Context, call Call) (core.Result, error) {
	if !call.Owner {
		return core.Result{}, fmt.Errorf("event command %s used member ingress", call.Command.Verb())
	}
	switch call.Command.(type) {
	case domain.LinkHall, domain.LinkProve:
		return h.homeTransport.Do(ctx, call)
	case domain.LinkVerify:
		r, err := h.homeTransport.Do(ctx, call)
		if err != nil {
			return r, err
		}
		var verified domain.LinkVerifyResult
		if err := json.Unmarshal(r.Data, &verified); err != nil {
			return core.Result{}, err
		}
		verified.CredentialGen = 8
		verified.Admission = 42
		return domain.OK(verified)
	default:
		return h.handle(call.Command)
	}
}

func eventWireFrame[T any](t *testing.T, r *link.Reader, typ string) (T, bool) {
	t.Helper()
	var got T
	frame, err := r.Read()
	if err != nil || frame.Type != typ || json.Unmarshal(frame.Body, &got) != nil {
		t.Errorf("want frame %s, got %+v, %v", typ, frame, err)
		return got, false
	}
	return got, true
}

func TestLinkServeStreamsSnapshotCatchUpAndLiveEvents(t *testing.T) {
	var stream string
	checkPeer := func(peer domain.LinkEventPeer) error {
		if peer.Device != "dev-1" || peer.CredentialGen != 8 || peer.Admission != 42 || !link.ValidNonce(peer.Stream) ||
			(stream != "" && stream != peer.Stream) {
			return fmt.Errorf("wrong event command peer: %+v", peer)
		}
		stream = peer.Stream
		return nil
	}
	wantLive := false
	closed := false
	ackCount := 0
	home := &eventHomeTransport{handle: func(cmd domain.Command) (core.Result, error) {
		switch in := cmd.(type) {
		case domain.LinkEvents:
			if err := checkPeer(in.LinkEventPeer); err != nil {
				return core.Result{}, err
			}
			switch {
			case in.Close:
				closed = true
				return domain.OK(domain.LinkEventsResult{})
			case in.Subscribe != nil && in.Subscribe.Boot == "boot" && in.Subscribe.FromSeq == 9 && in.Subscribe.Snapshot:
				return domain.OK(domain.LinkEventsResult{Snapshot: &link.Snapshot{Seq: 10}, Through: 10})
			case in.SnapshotPart != nil && *in.SnapshotPart == 1:
				return domain.OK(domain.LinkEventsResult{Snapshot: &link.Snapshot{Seq: 10, Part: 1, Last: true}, Through: 10})
			case in.FromSeq == 10:
				return domain.OK(domain.LinkEventsResult{Events: []link.Event{{Seq: 11, Kind: link.EventMembership}}, Through: 11})
			case in.FromSeq == 11 && wantLive:
				return domain.OK(domain.LinkEventsResult{Events: []link.Event{{Seq: 12, Kind: link.EventOffer}}, Through: 12})
			case in.FromSeq == 11 || in.FromSeq == 12:
				return domain.OK(domain.LinkEventsResult{Through: in.FromSeq})
			}
		case domain.LinkAck:
			if err := checkPeer(in.LinkEventPeer); err != nil {
				return core.Result{}, err
			}
			if in.Ack.Boot == "boot" && in.Ack.Seq == uint64(10+ackCount) {
				ackCount++
				return core.Result{Status: core.ExitOK}, nil
			}
		case domain.LinkFact:
			if err := checkPeer(in.LinkEventPeer); err != nil {
				return core.Result{}, err
			}
			if in.Fact.ID == 20 {
				return domain.OK(link.Recorded{ID: 20, Status: link.RecordedQuarantined})
			}
		case domain.LinkWritten:
			if err := checkPeer(in.LinkEventPeer); err != nil {
				return core.Result{}, err
			}
			if in.Written.ID == 21 {
				wantLive = true
				return domain.OK(link.Recorded{ID: 21, Status: link.RecordedApplied})
			}
		}
		return core.Result{}, fmt.Errorf("unexpected event command: %+v", cmd)
	}}
	stderr, code := serve(t, Env{Transport: home, ExperimentalLink: true}, func(r *link.Reader, w *link.Writer) {
		write := func(typ string, body any) bool {
			if err := w.Write(typ, body); err != nil {
				t.Error(err)
				return false
			}
			return true
		}
		spoke := spokeConfig("dev-1")
		spoke.Caps = []string{link.CapEvents, link.CapWake}
		session, err := spoke.Connect(r, w)
		if err != nil || !slices.Equal(session.Caps, []string{link.CapEvents}) {
			t.Errorf("event handshake: %+v, %v", session, err)
			return
		}
		if !write(link.TypeSubscribe, link.Subscribe{FromSeq: 9, Boot: "boot", Snapshot: true}) {
			return
		}
		first, ok := eventWireFrame[link.Snapshot](t, r, link.TypeSnapshot)
		if !ok || first.Seq != 10 || first.Part != 0 || first.Last {
			t.Errorf("first snapshot: %+v", first)
			return
		}
		last, ok := eventWireFrame[link.Snapshot](t, r, link.TypeSnapshot)
		if !ok || last.Seq != 10 || last.Part != 1 || !last.Last {
			t.Errorf("last snapshot: %+v", last)
			return
		}
		caught, ok := eventWireFrame[link.CaughtUp](t, r, link.TypeCaughtUp)
		if !ok || caught.Seq != 10 {
			t.Errorf("caught up: %+v", caught)
			return
		}
		if !write(link.TypeAck, link.Ack{Seq: 10, Boot: "boot"}) {
			return
		}
		event, ok := eventWireFrame[link.Event](t, r, link.TypeEvent)
		if !ok || event.Seq != 11 || event.Kind != link.EventMembership {
			t.Errorf("post-snapshot event: %+v", event)
			return
		}
		if !write(link.TypeAck, link.Ack{Seq: 11, Boot: "boot"}) ||
			!write(link.TypeFact, json.RawMessage(`{"id":20,"device":"another","credential_gen":99}`)) {
			return
		}
		fact, ok := eventWireFrame[link.Recorded](t, r, link.TypeRecorded)
		if !ok || fact.ID != 20 || fact.Status != link.RecordedQuarantined {
			t.Errorf("recorded fact: %+v", fact)
			return
		}
		if !write(link.TypeWritten, link.Written{ID: 21}) {
			return
		}
		written, ok := eventWireFrame[link.Recorded](t, r, link.TypeRecorded)
		if !ok || written.ID != 21 || written.Status != link.RecordedApplied {
			t.Errorf("recorded written: %+v", written)
			return
		}
		event, ok = eventWireFrame[link.Event](t, r, link.TypeEvent)
		if !ok || event.Seq != 12 || event.Kind != link.EventOffer {
			t.Errorf("live event: %+v", event)
		}
	})
	if code != core.ExitOK || stderr != "" || ackCount != 2 || !closed {
		t.Fatalf("exit %d, stderr %q, acknowledgements %d, closed %t", code, stderr, ackCount, closed)
	}
}

func TestLinkServeRejectsEventsWithoutNegotiation(t *testing.T) {
	stderr, code := serve(t, Env{Transport: &homeTransport{}, ExperimentalLink: true}, func(r *link.Reader, w *link.Writer) {
		session, err := spokeConfig("dev-1").Connect(r, w)
		if err != nil || len(session.Caps) != 0 {
			t.Errorf("handshake: %+v, %v", session, err)
			return
		}
		if err := w.Write(link.TypeSubscribe, link.Subscribe{Boot: "boot"}); err != nil {
			t.Error(err)
		}
	})
	if code == core.ExitOK || !strings.Contains(stderr, `no "subscribe" frame`) {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestLinkServeRejectsAckOutsideDeliveredRangeOrMultipart(t *testing.T) {
	cases := []struct {
		stream servedLinkEvents
		ack    link.Ack
	}{
		{servedLinkEvents{}, link.Ack{Boot: "boot"}},
		{servedLinkEvents{boot: "boot", sent: 5}, link.Ack{Boot: "other", Seq: 4}},
		{servedLinkEvents{boot: "boot", sent: 5}, link.Ack{Boot: "boot", Seq: 6}},
		{servedLinkEvents{boot: "boot", sent: 5, snapshotPart: 1}, link.Ack{Boot: "boot", Seq: 5}},
		{servedLinkEvents{boot: "boot", sent: 5, ackSeq: 4}, link.Ack{Boot: "boot", Seq: 3}},
		{servedLinkEvents{boot: "boot", sent: 5, ackSeq: 4, ackMore: true}, link.Ack{Boot: "boot", Seq: 5}},
	}
	for _, tc := range cases {
		if err := tc.stream.ack(t.Context(), Env{}, tc.ack); err == nil {
			t.Errorf("accepted %+v after %+v", tc.ack, tc.stream)
		}
	}
}

func TestLinkAckSplitsTheOwnerRequestAndKeepsTheFrameMoreBit(t *testing.T) {
	key := strings.Repeat("x", (link.MaxFrame-700)/2)
	in := domain.LinkAck{Ack: link.Ack{Boot: "boot", Seq: 3, Cuts: []link.LeafCut{
		{Member: link.Member{Vendor: "codex", Key: key}, Seq: 2},
		{Member: link.Member{Vendor: "claude", Key: key}, Seq: 3},
	}}, LinkEventPeer: domain.LinkEventPeer{Device: "dev", CredentialGen: 1, Stream: link.NewNonce()}}
	for _, more := range []bool{false, true} {
		in.Ack.More = more
		if err := link.NewWriter(&bytes.Buffer{}).Write(link.TypeAck, in.Ack); err != nil {
			t.Fatalf("test acknowledgement must fit on the link: %v", err)
		}
		parts, err := splitLinkAck(in)
		if err != nil || len(parts) != 2 || !parts[0].Ack.More || parts[1].Ack.More != more {
			t.Fatalf("split with More=%t: %d parts, %v", more, len(parts), err)
		}
		for i, part := range parts {
			if part.LinkEventPeer != in.LinkEventPeer || part.Ack.Boot != in.Ack.Boot || part.Ack.Seq != in.Ack.Seq ||
				!slices.Equal(part.Ack.Cuts, in.Ack.Cuts[i:i+1]) {
				t.Fatalf("part %d changed the acknowledgement", i)
			}
		}
	}
}

func TestLinkServeEventsReportsStaleAdmission(t *testing.T) {
	home := &eventHomeTransport{handle: func(cmd domain.Command) (core.Result, error) {
		if in, ok := cmd.(domain.LinkEvents); ok && in.Subscribe != nil {
			return domain.OK(domain.LinkEventsResult{Snapshot: &link.Snapshot{Last: true}})
		}
		return domain.Refuse(domain.CodeLinkStale, "stale admission", "reconnect the link"), nil
	}}
	stderr, code := serve(t, Env{Transport: home, ExperimentalLink: true}, func(r *link.Reader, w *link.Writer) {
		spoke := spokeConfig("dev-1")
		spoke.Caps = []string{link.CapEvents}
		if _, err := spoke.Connect(r, w); err != nil {
			t.Error(err)
			return
		}
		if err := w.Write(link.TypeSubscribe, link.Subscribe{Boot: "boot"}); err != nil {
			t.Error(err)
			return
		}
		if _, ok := eventWireFrame[link.Snapshot](t, r, link.TypeSnapshot); !ok {
			return
		}
		if _, ok := eventWireFrame[link.CaughtUp](t, r, link.TypeCaughtUp); !ok {
			return
		}
		if _, err := r.Read(); err == nil {
			t.Error("stale admission did not close the link")
		}
	})
	if code != core.ExitRefused || !strings.Contains(stderr, "reconnect the link") || !strings.Contains(stderr, "fences were pruned") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
