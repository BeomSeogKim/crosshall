package domain

import (
	"slices"
	"testing"
	"time"
)

func TestDepartedBindingDoesNotShowActivityFromTheDestination(t *testing.T) {
	f, writer, member, owed, unread := clearFixture(t, true)
	f.pickUp(member, unread)
	wantOK(t, f.reply(member, Reply{To: unread, Body: "settled in checkout"}, "settle-checkout"))
	f.hook(member, prompt("pid-7-a", "source-turn", f.now.UnixMilli()+1))
	f.now = f.now.Add(3 * time.Second)
	wantOK(t, f.run(member, Inbox{}, ""))
	sourceCommandAt := f.now
	waiting := decode[WaitingResult](t, f.run(writer, Waiting{}, ""))
	sourceRecipient := recipientOf(t, waiting.Requests, owed)
	if sourceRecipient.Observed == nil || sourceRecipient.Observed.Fact != FactPromptSubmitted || sourceRecipient.PickedUpAtMS == nil {
		t.Fatalf("live recipient's source activity: %+v", sourceRecipient)
	}
	cursor := decode[JoinResult](t, f.join(writer, Join{Channel: "checkout"}, "")).RosterSeq
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))

	f.now = f.now.Add(2 * time.Second)
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	f.now = f.now.Add(5 * time.Second)
	f.hook(member, prompt("pid-7-a", "destination-turn", f.now.UnixMilli()+1))
	f.hook(member, stop("pid-7-a", "destination-turn", f.now.UnixMilli()+1))
	wantOK(t, f.run(member, Inbox{}, ""))
	if m := f.memberRow(member, "moved"); m.Observed == nil || m.Observed.Fact != FactTurnFinishing || m.LastCommandAgoS != 0 {
		t.Fatalf("destination member activity: %+v", m)
	}

	waiting = decode[WaitingResult](t, f.run(writer, Waiting{}, ""))
	status := decode[StatusResult](t, f.run(writer, Status{}, ""))
	for view, requests := range map[string][]LedgerRequest{
		"waiting": waiting.Requests,
		"status":  status.Channels[0].Requests,
	} {
		r := recipientOf(t, requests, owed)
		if r.State != "open" || r.BindingState != "departed" || r.Observed != nil {
			t.Errorf("%s source recipient: %+v", view, r)
		}
		if r.StoredAtMS != sourceRecipient.StoredAtMS || r.PickedUpAtMS == nil || *r.PickedUpAtMS != *sourceRecipient.PickedUpAtMS ||
			!slices.Equal(r.Receipts, sourceRecipient.Receipts) {
			t.Errorf("%s source receipt times changed: %+v", view, r)
		}
	}
	if r := recipientOf(t, status.Channels[0].Requests, unread); r.State != "replied" || r.SettledAtMS == nil || r.Observed != nil {
		t.Errorf("settled source recipient: %+v", r)
	}
	delta := decode[JoinResult](t, f.joinAck(writer, Join{Channel: "checkout"}, cursor)).Changes
	if len(delta) != 1 || delta[0].Alias != "claude1" || delta[0].Change != "departed" {
		t.Fatalf("source roster changes: %+v", delta)
	}
	if age := int64(f.now.Sub(sourceCommandAt) / time.Second); delta[0].Observed != nil || delta[0].LastCommandAgoS != age {
		t.Errorf("source roster departure: %+v, want command age %d and no current observation", delta[0], age)
	}
	var source, destination *OwnerMember
	owner := decode[OwnerWhoResult](t, f.owner(OwnerWho{}, ""))
	for _, channel := range owner.Channels {
		for i := range channel.Members {
			m := &channel.Members[i]
			switch {
			case channel.Name == "checkout" && m.Alias == "claude1":
				source = m
			case channel.Name == "review" && m.Alias == "moved":
				destination = m
			}
		}
	}
	if source == nil || source.BindingState != "departed" || source.Observed != nil ||
		source.LastEvidence == nil || source.LastEvidence.AtMS != sourceCommandAt.UnixMilli() {
		t.Errorf("owner's source roster entry: %+v", source)
	}
	if destination == nil || destination.BindingState != "live" || destination.Observed == nil ||
		destination.Observed.Fact != FactTurnFinishing || destination.LastEvidence == nil ||
		destination.LastEvidence.AtMS != f.now.UnixMilli() {
		t.Errorf("owner's destination roster entry: %+v", destination)
	}
}
