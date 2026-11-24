//go:build e2e

package e2e

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func readWorkWait(t *testing.T, result waitExit, wantReason string) waitMail {
	t.Helper()
	mail := readWaitMail(t, result)
	var metadata struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &metadata); err != nil || metadata.Reason != wantReason {
		t.Fatalf("wait reason = %q, want %q: %v, %s", metadata.Reason, wantReason, err, result.stdout)
	}
	return mail
}

func awaitWorkWait(t *testing.T, child <-chan waitExit, reason string) waitMail {
	t.Helper()
	select {
	case result := <-child:
		return readWorkWait(t, result, reason)
	case <-time.After(fixtureEventTimeout):
		t.Fatal("wait did not report matching work")
		return waitMail{}
	}
}

// Watch a private fixture's mail leaf until the wait has read its first query
// response. The dispatcher stages a response inside out before renaming it,
// so a change alone can precede the response; the wait removes the file it
// read, which leaves out changed and empty.
func startWaitAfterInitialQuery(t *testing.T, member *participant, args ...string) <-chan waitExit {
	t.Helper()
	_, child := startWaitProcessAfterInitialQuery(t, member, args...)
	return child
}

func startWaitProcessAfterInitialQuery(t *testing.T, member *participant, args ...string) (*os.Process, <-chan waitExit) {
	t.Helper()
	path := filepath.Join(member.leaf(), "out")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	process, child := startWaitProcess(t, member, args...)
	timer := time.NewTimer(fixtureEventTimeout)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case result := <-child:
			t.Fatalf("wait exited before its first empty snapshot: %v, stdout %q, stderr %q", result.err, result.stdout, result.stderr)
		case <-timer.C:
			t.Fatalf("wait never read its first query response in %s", path)
		case <-tick.C:
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.ModTime().Equal(before.ModTime()) {
				continue
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) == 0 {
				return process, child
			}
		}
	}
}

func assertWorkItem(t *testing.T, rows []domain.InboxItem, id int64, kind string) {
	t.Helper()
	if len(rows) != 1 || rows[0].ID != id || rows[0].Kind != kind {
		t.Fatalf("wait rows = %+v, want only %s #%d", rows, kind, id)
	}
}

func workPickup(t *testing.T, db *sql.DB, id int64) (sql.NullInt64, int) {
	t.Helper()
	var pickedUp sql.NullInt64
	var receipts int
	if err := db.QueryRow(`SELECT r.picked_up_at_ms,
		(SELECT COUNT(*) FROM receipt p WHERE p.message_id = r.message_id
		 AND p.principal_id = r.principal_id AND p.stage = 'picked_up')
		FROM recipient r WHERE r.message_id = ?`, id).Scan(&pickedUp, &receipts); err != nil {
		t.Fatal(err)
	}
	return pickedUp, receipts
}

func assertWorkUnread(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	if pickup, receipts := workPickup(t, db, id); pickup.Valid || receipts != 0 {
		t.Errorf("wait picked up #%d: timestamp %v, receipts %d", id, pickup, receipts)
	}
}

func TestWaitExistingWorkClosesInboxReplyHandoff(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	ask := num(t, form.must("ask", "@tax", "check the totals"), "id")
	inbox := form.must("inbox")
	if len(ids(inbox.obj["unread"])) != 0 {
		t.Fatalf("inbox should be empty before the other member replies: %s", inbox.stdout)
	}
	reply := num(t, tax.must("reply", strconv.FormatInt(ask, 10), "the totals match"), "id")
	note := num(t, tax.must("note", "@form", "a note is not work"), "id")
	db := w.db()
	before := waitMemberState(t, db, form, "form")

	// The reply was committed between the empty inbox and this process's
	// initial snapshot, where a wait without --existing would put it behind
	// its arrival cursor.
	result := form.must("wait", "--existing", "--work-only", "--timeout", "1s")
	mail := readWorkWait(t, waitExit{stdout: result.stdout, stderr: result.stderr}, "existing")
	assertWorkItem(t, mail.Unread, reply, "reply")
	if len(mail.Owed) != 0 {
		t.Errorf("a reply did not create an obligation: %+v", mail.Owed)
	}
	plain := strings.ToLower(form.mustPlain("wait", "--existing", "--work-only", "--timeout", "1s"))
	if !strings.HasPrefix(plain, "already in your inbox\n") {
		t.Errorf("plain wait did not describe the existing work: %q", plain)
	}
	assertWorkUnread(t, db, reply)
	assertWorkUnread(t, db, note)
	if after := waitMemberState(t, db, form, "form"); after != before {
		t.Errorf("existing wait changed member state: before %+v, after %+v", before, after)
	}

	second := num(t, form.must("ask", "@tax", "check the next calculation"), "id")
	child := startWaitAfterInitialQuery(t, form, "--work-only", "--timeout", "5s")
	tax.must("note", "@form", "skip this even without --existing")
	requireWaiting(t, child, 230*time.Millisecond)
	newReply := num(t, tax.must("reply", strconv.FormatInt(second, 10), "the calculation matches"), "id")
	mail = awaitWorkWait(t, child, "")
	assertWorkItem(t, mail.Unread, newReply, "reply")
	assertWorkUnread(t, db, newReply)
}

func TestWaitExistingWorkClosesRearmGap(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	child := startWaitAfterInitialQuery(t, tax, "--timeout", "5s")
	note := num(t, form.must("note", "@tax", "first wake"), "id")
	assertWorkItem(t, awaitWorkWait(t, child, "").Unread, note, "note")
	tax.must("read", strconv.FormatInt(note, 10))

	ask := num(t, form.must("ask", "@tax", "sent before the next wait starts"), "id")
	result := tax.must("wait", "--existing", "--work-only", "--timeout", "1s")
	mail := readWorkWait(t, waitExit{stdout: result.stdout, stderr: result.stderr}, "existing")
	assertWorkItem(t, mail.Unread, ask, "ask")
	assertWorkUnread(t, w.db(), ask)
}

func TestWaitExistingOwedRequestDoesNotWakeWait(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	ask := num(t, form.must("ask", "@tax", "review the calculation"), "id")
	tax.must("read", strconv.FormatInt(ask, 10))
	db := w.db()
	oldPickup, oldReceipts := workPickup(t, db, ask)
	if !oldPickup.Valid || oldReceipts != 1 {
		t.Fatalf("fixture must already owe ask #%d: %v, receipts %d", ask, oldPickup, oldReceipts)
	}

	for _, filter := range [][]string{nil, {"--work-only"}} {
		args := append([]string{"wait", "--existing", "--timeout", "250ms"}, filter...)
		result := tax.run(args...)
		if result.exit != 6 || result.obj["code"] != "wait_timeout" {
			t.Errorf("wait over only owed work with %v: exit %d, %s", args, result.exit, result.stdout)
		}
	}

	child := startWaitAfterInitialQuery(t, tax, "--existing", "--work-only", "--timeout", "5s")
	fresh := num(t, form.must("ask", "@tax", "review the other calculation"), "id")
	mail := awaitWorkWait(t, child, "arrived")
	assertWorkItem(t, mail.Unread, fresh, "ask")
	assertWorkItem(t, mail.Owed, ask, "ask")

	result := tax.must("wait", "--existing", "--work-only", "--timeout", "1s")
	mail = readWorkWait(t, waitExit{stdout: result.stdout, stderr: result.stderr}, "existing")
	assertWorkItem(t, mail.Unread, fresh, "ask")
	assertWorkItem(t, mail.Owed, ask, "ask")
	if pickup, receipts := workPickup(t, db, ask); pickup != oldPickup || receipts != oldReceipts {
		t.Errorf("wait changed the picked-up request: timestamp %v, receipts %d", pickup, receipts)
	}
	assertWorkUnread(t, db, fresh)
}

func TestWaitExistingWorkIgnoresNotesUntilARequestArrives(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	old := num(t, form.must("note", "@tax", "earlier note"), "id")
	db := w.db()
	before := waitMemberState(t, db, tax, "tax")
	child := startWaitAfterInitialQuery(t, tax, "--existing", "--work-only", "--timeout", "5s")
	note := num(t, form.must("note", "@tax", "note arrives during the wait"), "id")
	requireWaiting(t, child, 230*time.Millisecond)
	ask := num(t, form.must("ask", "@tax", "new request during the wait"), "id")
	mail := awaitWorkWait(t, child, "arrived")
	assertWorkItem(t, mail.Unread, ask, "ask")
	if len(mail.Owed) != 0 {
		t.Errorf("the untouched request was already shown as owed: %+v", mail.Owed)
	}
	for _, id := range []int64{old, note, ask} {
		assertWorkUnread(t, db, id)
	}
	if after := waitMemberState(t, db, tax, "tax"); after != before {
		t.Errorf("arrival wait changed member state: before %+v, after %+v", before, after)
	}
}

func TestWaitExistingAnyStartsWithTheCallersInbox(t *testing.T) {
	_, _, members := setup(t)
	form, tax := members[0], members[1]
	tax.must("note", "@lint", "third-party history")
	child := startWaitAfterInitialQuery(t, form, "--existing", "--any", "--timeout", "5s")
	note := num(t, tax.must("note", "@lint", "new third-party traffic"), "id")
	mail := awaitWorkWait(t, child, "arrived")
	assertWorkItem(t, mail.Unread, note, "note")
}

func TestWaitExistingWorkOnlyTimeoutAndFlagConflict(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	note := num(t, form.must("note", "@tax", "the only mail is a note"), "id")
	result := tax.must("wait", "--existing", "--timeout", "1s")
	mail := readWorkWait(t, waitExit{stdout: result.stdout, stderr: result.stderr}, "existing")
	assertWorkItem(t, mail.Unread, note, "note")
	result = tax.run("wait", "--existing", "--work-only", "--timeout", "200ms")
	if result.exit != 6 || result.obj["code"] != "wait_timeout" {
		t.Errorf("wait over only notes: exit %d, %s", result.exit, result.stdout)
	}
	result = tax.run("wait", "--existing", "--work-only", "--any", "--timeout", "200ms")
	if result.exit != 2 || result.obj["code"] != "usage" {
		t.Errorf("wait with --work-only and --any: exit %d, %s", result.exit, result.stdout)
	}
	assertWorkUnread(t, w.db(), note)
}
