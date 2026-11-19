package domain

import (
	"fmt"
	"slices"
	"testing"
)

func itemIDs(items []InboxItem) []int64 {
	ids := []int64{}
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// A request the owner cancelled before its recipient read it is shown apart,
// once, until it is read: it is not unread work, so neither the pending line,
// the turn-end check nor a wait for existing work treats it as a request.
func TestCancelledUnreadRequestIsShownApartAndContinuesNothing(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	cancelled := f.turnEndAsk(writer, "reader", "cancelled")
	open := f.turnEndAsk(writer, "reader", "open")
	wantOK(t, f.owner(Cancel{Channel: "checkout", ID: cancelled}, "cancel"))

	inbox := decode[InboxResult](t, f.run(reader, Inbox{}, ""))
	if got := itemIDs(inbox.Unread); !slices.Equal(got, []int64{open}) {
		t.Fatalf("unread %v, want only the open request %d", got, open)
	}
	if got := itemIDs(inbox.Cancelled); !slices.Equal(got, []int64{cancelled}) {
		t.Fatalf("cancelled %v, want %d", got, cancelled)
	}

	at := f.now.UnixMilli() + 1
	if r := f.hook(reader, prompt("launch", "turn-2", at)); r.PendingLine != pendingLine(open) {
		t.Fatalf("prompt pending line %q, want only %d", r.PendingLine, open)
	}
	if r := f.hook(reader, stop("launch", "turn-2", at+1)); r.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", open) {
		t.Fatalf("turn-end line %q, want only %d", r.TurnEndLine, open)
	}

	wantOK(t, f.run(reader, Read{IDs: []int64{open}}, "read-open"))
	snapshot := decode[WaitSnapshot](t, f.run(reader, Wait{Existing: true}, ""))
	if len(snapshot.Unread) != 0 {
		t.Fatalf("a cancelled request alone ends a wait for existing work: %+v", snapshot.Unread)
	}

	wantOK(t, f.run(reader, Read{IDs: []int64{cancelled}}, "read-cancelled"))
	if after := decode[InboxResult](t, f.run(reader, Inbox{}, "")); len(after.Cancelled) != 0 {
		t.Fatalf("a read cancellation is still listed: %v", itemIDs(after.Cancelled))
	}
}

// A wait armed before the ask does not wake for it when the owner cancels it
// before the next poll, and the cursor still moves past it.
func TestWaitIgnoresARequestCancelledAfterTheCursor(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	first := decode[WaitSnapshot](t, f.run(reader, Wait{}, ""))
	id := f.turnEndAsk(writer, "reader", "later-cancelled")
	wantOK(t, f.owner(Cancel{Channel: "checkout", ID: id}, "cancel"))
	cursor := first.Cursor
	next := decode[WaitSnapshot](t, f.run(reader, Wait{Cursor: &cursor}, ""))
	if len(next.Unread) != 0 || len(next.Cancelled) != 0 {
		t.Fatalf("a cancelled request woke the wait: %+v", next.InboxResult)
	}
	if next.Cursor.MessageID < id {
		t.Fatalf("cursor %d did not move past request %d", next.Cursor.MessageID, id)
	}
}

// An initial wait for existing work returns the unread work only: a
// cancellation never rides along with another arrival.
func TestWaitForExistingWorkLeavesCancellationsOut(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	cancelled := f.turnEndAsk(writer, "reader", "cancelled")
	wantOK(t, f.owner(Cancel{Channel: "checkout", ID: cancelled}, "cancel"))
	open := f.turnEndAsk(writer, "reader", "open")
	got := decode[WaitSnapshot](t, f.run(reader, Wait{Existing: true}, ""))
	if ids := itemIDs(got.Unread); !slices.Equal(ids, []int64{open}) || len(got.Cancelled) != 0 {
		t.Fatalf("wait reply unread %v cancelled %v, want only %d", ids, itemIDs(got.Cancelled), open)
	}
}
