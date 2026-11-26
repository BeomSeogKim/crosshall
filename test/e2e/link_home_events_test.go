//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func readHomeEventFrame(t *testing.T, s *servedLink) link.Frame {
	t.Helper()
	type result struct {
		frame link.Frame
		err   error
	}
	read := make(chan result, 1)
	go func() {
		frame, err := s.r.Read()
		read <- result{frame, err}
	}()
	select {
	case r := <-read:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.frame
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading the home event stream")
		return link.Frame{}
	}
}

func writeHomeEventFrame(t *testing.T, s *servedLink, kind string, value any) {
	t.Helper()
	if err := s.w.Write(kind, value); err != nil {
		t.Fatal(err)
	}
}

func TestLinkHomeStreamsAndConfirmsPreparedOffer(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	policy := owner.must("owner", "adapter", "set", "claude", "--enabled", "on")
	epoch, ok := policy.obj["epoch"].(float64)
	if !ok || epoch <= 0 {
		t.Fatalf("adapter policy: %s", policy.stdout)
	}
	member := link.Member{Vendor: "claude", Key: "linked-fixture"}
	secret := bytes.Repeat([]byte{0x42}, 32)
	hall, device := enrollDevice(t, w, owner, "spoke", secret)
	s := serveLink(t, w, owner, link.Spoke{Target: "home", Hall: hall, Device: device, Secret: secret,
		Release: "test", Link: link.Supported(), Caps: []string{link.CapEvents}})
	joined := s.call(t, domain.LinkCall{Vendor: member.Vendor, Key: member.Key, LeafGeneration: "g1", LeafEpoch: 1,
		Command: "join", Args: json.RawMessage(`{"channel":"checkout","alias":"remote"}`), OpKey: "join"})
	if joined.Status != core.ExitOK {
		t.Fatalf("remote join: %+v", joined)
	}

	const boot = "spoke-boot"
	writeHomeEventFrame(t, s, link.TypeSubscribe, link.Subscribe{Boot: boot})
	frame := readHomeEventFrame(t, s)
	var snapshot link.Snapshot
	if frame.Type != link.TypeSnapshot || json.Unmarshal(frame.Body, &snapshot) != nil ||
		!snapshot.Last || len(snapshot.Members) != 1 || snapshot.Members[0].Member != member {
		t.Fatalf("remote snapshot: %+v", frame)
	}
	scope := snapshot.Members[0].Scope
	if scope.LeafGeneration != "g1" || scope.Membership.BindingID <= 0 {
		t.Fatalf("remote scope: %+v", scope)
	}
	if f := readHomeEventFrame(t, s); f.Type != link.TypeCaughtUp {
		t.Fatalf("snapshot boundary: %+v", f)
	}
	writeHomeEventFrame(t, s, link.TypeAck, link.Ack{Seq: snapshot.Seq, Boot: boot,
		Cuts: []link.LeafCut{{Member: member, Seq: snapshot.Seq, Scope: scope}}})
	token := link.HookToken{Boot: boot, LeafSeq: 1, LeafGeneration: "g1", Membership: scope.Membership}
	profile := wire.HookProfile{Adapter: "claude", ManifestRev: "claude-3", VendorVersion: "2.1.331",
		Platform: "darwin-arm64", Mode: "interactive"}
	body, err := json.Marshal(wire.HookEnrollArgs{Profile: profile, LaunchRef: "spoke-launch",
		Source: "startup", EventRef: "spoke-start", HookAtMS: 1, Epoch: uint64(epoch)})
	if err != nil {
		t.Fatal(err)
	}
	writeHomeEventFrame(t, s, link.TypeFact, link.Fact{ID: 20, Member: member, Token: token, Kind: "unknown", Body: body})
	frame = readHomeEventFrame(t, s)
	var recorded link.Recorded
	if frame.Type != link.TypeRecorded || json.Unmarshal(frame.Body, &recorded) != nil ||
		recorded.ID != 20 || recorded.Status != link.RecordedStale || recorded.Code != "invalid_input" {
		t.Fatalf("invalid fact recording: %+v, %+v", frame, recorded)
	}
	writeHomeEventFrame(t, s, link.TypeFact, link.Fact{ID: 1, Member: member, Token: token, Kind: wire.VerbHookEnroll, Body: body})
	frame = readHomeEventFrame(t, s)
	recorded = link.Recorded{}
	if frame.Type != link.TypeRecorded || json.Unmarshal(frame.Body, &recorded) != nil ||
		recorded.ID != 1 || recorded.Status != link.RecordedApplied {
		t.Fatalf("launch recording: %+v, %+v", frame, recorded)
	}
	local := w.member()
	local.must("join", "checkout", "--as", "local")
	const privateMessage = "test message stays in the ledger"
	note := local.must("note", "@remote", privateMessage)
	noteID, ok := note.obj["id"].(float64)
	if !ok || noteID <= 0 {
		t.Fatalf("note: %s", note.stdout)
	}

	var cuts []link.LeafCut
	var offer link.Offer
	var offerSeq uint64
	for i := 0; i < 10 && offerSeq == 0; i++ {
		frame := readHomeEventFrame(t, s)
		var event link.Event
		if frame.Type != link.TypeEvent || json.Unmarshal(frame.Body, &event) != nil {
			t.Fatalf("live event: %+v", frame)
		}
		switch event.Kind {
		case link.EventInvalidate, link.EventMembership:
			var change link.MembershipChange
			if err := json.Unmarshal(event.Body, &change); err != nil || change.Member != member {
				t.Fatalf("unexpected scope change: %+v, %v", change, err)
			}
			cuts = append(cuts, link.LeafCut{Member: member, Scope: change.Scope, Seq: event.Seq, LeafSeq: 1})
		case link.EventOffer:
			if err := json.Unmarshal(event.Body, &offer); err != nil {
				t.Fatal(err)
			}
			offerSeq = event.Seq
			if bytes.Contains(event.Body, []byte(privateMessage)) {
				t.Fatal("a prepared offer leaked the message body")
			}
		}
	}
	if offerSeq == 0 || offer.Member != member || offer.Launch != "spoke-launch" || offer.Nonce == "" ||
		len(offer.IDs) != 1 || offer.IDs[0] != int64(noteID) || offer.LeaseMS <= 0 || offer.Membership != scope.Membership {
		t.Fatalf("prepared offer: %+v", offer)
	}
	writeHomeEventFrame(t, s, link.TypeAck, link.Ack{Seq: offerSeq, Boot: boot, Cuts: cuts})
	token.LeafSeq = 2
	writeHomeEventFrame(t, s, link.TypeWritten, link.Written{ID: 2, Member: member,
		OfferID: offer.OfferID, Nonce: offer.Nonce, WrittenAtMS: 1, Token: token})
	frame = readHomeEventFrame(t, s)
	if frame.Type != link.TypeRecorded || json.Unmarshal(frame.Body, &recorded) != nil ||
		recorded.ID != 2 || recorded.Status != link.RecordedApplied {
		t.Fatalf("written recording: %+v, %+v", frame, recorded)
	}
}
