package domain

import (
	"encoding/json"
	"testing"
)

func TestWaitSnapshotFindsExistingWorkAndSkipsNotesReadOnly(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, ""))
	writer := ledgerMember(t, f, "checkout", "writer")
	reader := ledgerMember(t, f, "checkout", "reader")
	principal := ledgerCaller(t, f, reader)

	wait, err := Decode(VerbWait, json.RawMessage(`{"existing":true,"work_only":true}`))
	if err != nil {
		t.Fatal(err)
	}
	cmd := wait.(Wait)
	initial := decode[InboxResult](t, actLedger(f, reader, Inbox{}, ""))
	if len(initial.Unread)+len(initial.Owed) != 0 {
		t.Fatalf("expected empty inbox before send: %+v", initial)
	}

	owed, _ := seedAsk(t, f, "checkout", "already opened", writer, reader)
	wantOK(t, actLedger(f, reader, Read{IDs: []int64{owed}}, "pickup"))
	unread, _ := seedAsk(t, f, "checkout", "sent in the gap", writer, reader)
	wantOK(t, actLedger(f, writer, Note{To: []string{"reader"}, Body: "ignored"}, "note-before"))
	before := f.count("SELECT COUNT(*) FROM observation WHERE principal_id = ?", principal)
	r := decode[WaitSnapshot](t, actLedger(f, reader, cmd, ""))
	if len(r.Unread) != 1 || r.Unread[0].ID != unread || len(r.Owed) != 1 || r.Owed[0].ID != owed {
		t.Fatalf("initial snapshot did not return exactly the unread and owed requests: %+v", r)
	}
	if r.Cursor.MessageID != int64(f.count("SELECT MAX(id) FROM message")) {
		t.Fatalf("inbox and cursor were not taken together: %+v", r.Cursor)
	}

	cmd.Cursor = &r.Cursor
	wantOK(t, actLedger(f, writer, Note{To: []string{"reader"}, Body: "still ignored"}, "note-after"))
	r = decode[WaitSnapshot](t, actLedger(f, reader, cmd, ""))
	if len(r.Unread)+len(r.Owed) != 0 {
		t.Fatalf("a future note or previously seen owed item ended a poll: %+v", r)
	}
	cmd.Cursor = &r.Cursor
	fresh, _ := seedAsk(t, f, "checkout", "arrived during the wait", writer, reader)
	r = decode[WaitSnapshot](t, actLedger(f, reader, cmd, ""))
	if len(r.Unread) != 1 || r.Unread[0].ID != fresh || len(r.Owed) != 1 || r.Owed[0].ID != owed {
		t.Fatalf("poll did not return the new request: %+v", r)
	}
	if after := f.count("SELECT COUNT(*) FROM observation WHERE principal_id = ?", principal); after != before {
		t.Fatalf("wait recorded activity: before %d, after %d", before, after)
	}
	if got := f.count("SELECT COUNT(*) FROM recipient WHERE principal_id = ? AND picked_up_at_ms IS NOT NULL", principal); got != 1 {
		t.Fatalf("wait changed or picked up requests: %d", got)
	}
}
