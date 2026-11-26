package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

func TestLinkServeWakeUsesNegotiatedPeerAndDurableResult(t *testing.T) {
	peer := domain.LinkEventPeer{Device: "dev", CredentialGen: 2, Admission: 5, Stream: link.NewNonce()}
	wake := link.Wake{AttemptID: 7, Route: link.WakeRoute{Lane: link.NewNonce()}}
	home := &eventHomeTransport{handle: func(cmd domain.Command) (core.Result, error) {
		switch in := cmd.(type) {
		case domain.LinkWakeReady:
			if in.LinkEventPeer != peer || in.Route != wake.Route {
				t.Fatalf("ready scope %+v", in)
			}
			return domain.OK(domain.LinkWakeReadyResult{Seq: 9, Replay: &wake})
		case domain.LinkWakeResult:
			if in.LinkEventPeer != peer || in.Result.AttemptID != wake.AttemptID {
				t.Fatalf("result scope %+v", in)
			}
			return domain.OK(link.WakeRecorded{AttemptID: in.Result.AttemptID, Status: link.RecordedApplied})
		default:
			t.Fatalf("unexpected wake command %+v", cmd)
			return core.Result{}, nil
		}
	}}
	env := Env{Transport: home}
	var wire bytes.Buffer
	writer := link.NewWriter(&wire)
	route, err := json.Marshal(wake.Route)
	if err != nil {
		t.Fatal(err)
	}
	ready := link.Frame{Type: link.TypeWakeReady, Body: route}
	stream := servedLinkEvents{peer: peer, caughtUp: true, sent: 8}
	if handled, err := stream.handleFrame(t.Context(), env, writer, ready); handled || err != nil {
		t.Fatalf("unsupported ready frame: handled=%t, error=%v", handled, err)
	}
	stream.allowWake = true
	if handled, err := stream.handleFrame(t.Context(), env, writer, ready); !handled || err != nil || !stream.more || wire.Len() != 0 {
		t.Fatalf("ready before its event: handled=%t, error=%v, more=%t, bytes=%d", handled, err, stream.more, wire.Len())
	}
	stream.sent, stream.more = 9, false
	if _, err := stream.handleFrame(t.Context(), env, writer, ready); err != nil {
		t.Fatal(err)
	}
	if replay, ok := eventWireFrame[link.Wake](t, link.NewReader(&wire), link.TypeWakeReplay); !ok || replay.AttemptID != wake.AttemptID {
		t.Fatalf("wake replay = %+v", replay)
	}
	body, err := json.Marshal(link.WakeResult{AttemptID: wake.AttemptID, Route: wake.Route, Outcome: link.WakeUncertain})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.handleFrame(t.Context(), env, writer, link.Frame{Type: link.TypeWakeResult, Body: body}); err != nil {
		t.Fatal(err)
	}
	if recorded, ok := eventWireFrame[link.WakeRecorded](t, link.NewReader(&wire), link.TypeWakeRecorded); !ok ||
		recorded.AttemptID != wake.AttemptID || recorded.Status != link.RecordedApplied {
		t.Fatalf("wake receipt = %+v", recorded)
	}
}
