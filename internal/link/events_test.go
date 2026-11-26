package link

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// Every event-stream frame crosses the link intact, as both halves of
// PR 6 build on these shapes.
func TestEventFramesRoundTrip(t *testing.T) {
	m := Member{Vendor: "claude", Key: "k1"}
	token := HookToken{Boot: "b1", LeafSeq: 7, MonoNS: 9, WallMS: 11, LeafGeneration: "g1", Membership: Membership{BindingID: 3, Generation: 2}}
	scope := MemberScope{LeafGeneration: "g1", Membership: Membership{BindingID: 3, Generation: 2}, RouteEpoch: 1}
	offer := Offer{Member: m, OfferID: 5, Nonce: "n", LeafGeneration: "g1", Membership: token.Membership, BindingID: 3, RouteEpoch: 1, PolicyEpoch: 4, Launch: "l", Epoch: 2, IDs: []int64{8, 9}, LeaseMS: 60000, LeaseEpoch: 2}
	body, _ := json.Marshal(offer)
	for typ, v := range map[string]any{
		TypeSubscribe: &Subscribe{FromSeq: 3, Boot: "b1", Snapshot: true},
		TypeAck:       &Ack{Seq: 4, Boot: "b1", Cuts: []LeafCut{{Member: m, Seq: 4, LeafSeq: 7, Scope: scope}, {Member: m, Seq: 4, Scope: scope, Absent: true}}, More: true},
		TypeCaughtUp:  &CaughtUp{Seq: 6},
		TypeRecorded:  &Recorded{ID: 2, Status: RecordedStale, Code: "stale_leaf"},
		TypeFact:      &Fact{ID: 1, Member: m, Token: token, Kind: "prompt", Body: json.RawMessage(`{"x":1}`)},
		TypeWritten:   &Written{ID: 2, Member: m, OfferID: 5, Nonce: "n", WrittenAtMS: 12, Token: token},
		TypeEvent:     &Event{Seq: 6, Kind: EventOffer, Body: body},
		TypeSnapshot: &Snapshot{Seq: 6, Part: 1, Last: true, Members: []SnapshotMember{{Member: m, Scope: scope,
			Offers: []Offer{offer}}}},
	} {
		var buf bytes.Buffer
		if err := NewWriter(&buf).Write(typ, v); err != nil {
			t.Fatal(err)
		}
		f, err := NewReader(&buf).Read()
		if err != nil || f.Type != typ {
			t.Fatalf("%s: %+v, %v", typ, f, err)
		}
		got := reflect.New(reflect.TypeOf(v).Elem()).Interface()
		if err := json.Unmarshal(f.Body, got); err != nil || !reflect.DeepEqual(got, v) {
			t.Fatalf("%s: got %+v, want %+v (%v)", typ, got, v, err)
		}
	}
	// A member is named flat in each body, beside the fields of its kind.
	if !bytes.Contains(body, []byte(`"vendor":"claude","key":"k1","offer_id":5`)) {
		t.Fatalf("offer body %s", body)
	}
}
