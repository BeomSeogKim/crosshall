//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

type waitExit struct {
	stdout string
	stderr string
	err    error
}

func startWait(t *testing.T, member *participant, args ...string) <-chan waitExit {
	t.Helper()
	_, result := startWaitProcess(t, member, args...)
	return result
}

func startWaitProcess(t *testing.T, member *participant, args ...string) (*os.Process, <-chan waitExit) {
	t.Helper()
	// Bound even the no-timeout invocation if a regression leaves it running.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, append([]string{"--json", "wait"}, args...)...)
	cmd.Dir, cmd.Env = member.w.home, member.env()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	result := make(chan waitExit, 1)
	go func() {
		err := cmd.Wait()
		result <- waitExit{out.String(), errOut.String(), err}
	}()
	return cmd.Process, result
}

func requireWaiting(t *testing.T, child <-chan waitExit, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case got := <-child:
		t.Fatalf("wait exited before a matching arrival: %v, stdout %q, stderr %q", got.err, got.stdout, got.stderr)
	case <-timer.C:
	}
}

type waitMail struct {
	Kind   string             `json:"kind"`
	OK     bool               `json:"ok"`
	Unread []domain.InboxItem `json:"unread"`
	Owed   []domain.InboxItem `json:"owed"`
}

func readWaitMail(t *testing.T, result waitExit) waitMail {
	t.Helper()
	var mail waitMail
	err := json.Unmarshal([]byte(result.stdout), &mail)
	if result.err != nil || err != nil || !mail.OK || mail.Kind != "wait" || mail.Unread == nil || mail.Owed == nil {
		t.Fatalf("wait did not return an inbox-shaped success: process %v, json %v, stdout %q, stderr %q", result.err, err, result.stdout, result.stderr)
	}
	return mail
}

func awaitWaitMail(t *testing.T, child <-chan waitExit) waitMail {
	t.Helper()
	select {
	case got := <-child:
		return readWaitMail(t, got)
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not report matching mail")
		return waitMail{}
	}
}

type waitingMemberState struct {
	rosterAck int64
	hintAck   bool
	events    int
	leafAck   string
}

func waitMemberState(t *testing.T, db *sql.DB, member *participant, alias string) waitingMemberState {
	t.Helper()
	var state waitingMemberState
	err := db.QueryRow(`SELECT b.roster_ack_seq, b.hint_acked,
	    (SELECT COUNT(*) FROM observation o WHERE o.principal_id = b.principal_id)
	    FROM binding b WHERE b.alias = ? AND b.state = 'live'`, alias).
		Scan(&state.rosterAck, &state.hintAck, &state.events)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := os.ReadFile(filepath.Join(member.leaf(), "ack"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	state.leafAck = string(ack)
	return state
}

func TestWaitReportsNewAddressedAskWithoutPickup(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	db := w.db()
	grantRoots(t, w, "tax", 1)
	tax.must("note", "@form", "older unread note")
	before := waitMemberState(t, db, form, "form")

	started := time.Now()
	child := startWait(t, form, "--timeout", "8s")
	// Deliver unrelated traffic after giving the first snapshot time to run.
	// Keep the waiter open past the normal three-second cold-start budget.
	requireWaiting(t, child, 200*time.Millisecond)
	tax.mustFixtureWrite("note", "@lint", "unrelated note while waiting")
	requireWaiting(t, child, 1200*time.Millisecond)
	tax.mustFixtureWrite("note", "@lint", "another unrelated note while waiting")
	requireWaiting(t, child, time.Until(started.Add(3100*time.Millisecond)))

	id := num(t, tax.must("ask", "@form", "new addressed ask", "--due", "10m"), "id")
	mail := awaitWaitMail(t, child)
	rows := mail.Unread
	if len(rows) != 1 || rows[0].ID != id || rows[0].Kind != "ask" || rows[0].Sender != "tax" || rows[0].DueAtMS == nil {
		t.Errorf("unread = %+v, want only new ask #%d from tax with a due time", rows, id)
	}
	if len(mail.Owed) != 0 {
		t.Errorf("unpicked request appeared in owed: %+v", mail.Owed)
	}

	var pickedUp sql.NullInt64
	var pickups int
	err := db.QueryRow(`SELECT picked_up_at_ms,
	    (SELECT COUNT(*) FROM receipt WHERE stage = 'picked_up' AND principal_id = r.principal_id AND message_id = r.message_id)
	    FROM recipient r WHERE message_id = ?`, id).Scan(&pickedUp, &pickups)
	if err != nil || pickedUp.Valid || pickups != 0 {
		t.Errorf("wait marked #%d picked up: timestamp %v, receipts %d, error %v", id, pickedUp, pickups, err)
	}
	if after := waitMemberState(t, db, form, "form"); after != before {
		t.Errorf("wait changed observation or roster acknowledgement: before %+v, after %+v", before, after)
	}

	replies := startWait(t, tax, "--timeout", "5s")
	requireWaiting(t, replies, 300*time.Millisecond)
	replyID := num(t, form.must("reply", strconv.FormatInt(id, 10), "new addressed reply"), "id")
	replyMail := awaitWaitMail(t, replies)
	if rows := replyMail.Unread; len(rows) != 1 || rows[0].ID != replyID || rows[0].Kind != "reply" || rows[0].Sender != "form" {
		t.Errorf("wait unread = %+v, want only new reply #%d from form", rows, replyID)
	}
}

func TestWaitAnyAcceptsThirdPartyTrafficAndIgnoresOwnSends(t *testing.T) {
	w, owner, members := setup(t)
	form, tax := members[0], members[1]
	owner.must("owner", "channel", "create", "separate")
	outside, south := w.member(), w.member()
	outside.must("join", "separate", "--as", "north")
	south.must("join", "separate", "--as", "south")
	tax.must("note", "@lint", "earlier third-party note")
	child := startWait(t, form, "--any")
	for range 2 {
		requireWaiting(t, child, 150*time.Millisecond)
		form.must("note", "@lint", "waiter's own send")
		outside.must("note", "@south", "another channel's note")
	}
	requireWaiting(t, child, 200*time.Millisecond)

	// Re-send a distinct candidate only if the first signal races initial
	// process startup or delivery. Every acceptable result must be from tax.
	posted := make(map[int64]bool)
	var got waitExit
	found := false
	for range 10 {
		want := tax.must("note", "@lint", "third-party note during the wait")
		posted[num(t, want, "id")] = true
		select {
		case got = <-child:
			found = true
		case <-time.After(150 * time.Millisecond):
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("--any did not wake up on another member's note")
	}
	mail := readWaitMail(t, got)
	if len(mail.Unread) == 0 || len(mail.Owed) != 0 {
		t.Fatalf("--any mail = %+v, want new third-party note rows and no obligation", mail)
	}
	for _, row := range mail.Unread {
		if !posted[row.ID] || row.Kind != "note" || row.Sender != "tax" {
			t.Errorf("--any returned an earlier or own send: %+v", row)
		}
	}
}

func TestWaitTimeoutAndEnrollmentRefusal(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	tax.must("note", "@form", "old unread mail must stay unread")
	started := time.Now()
	timed := form.run("wait", "--timeout", "200ms")
	if timed.exit != 6 || timed.obj["ok"] != false || timed.obj["code"] != "wait_timeout" ||
		!strings.Contains(timed.stdout, `"message"`) || time.Since(started) < 200*time.Millisecond {
		t.Fatalf("timed wait: exit %d, stdout %s, elapsed %v", timed.exit, timed.stdout, time.Since(started))
	}
	for _, duration := range []string{"0s", "-1s", "sometime"} {
		if out := form.run("wait", "--timeout", duration); out.exit != 2 || out.obj["code"] != "usage" {
			t.Errorf("wait timeout %q should be usage error: exit %d, %s", duration, out.exit, out.stdout)
		}
	}
	if out := w.member().run("wait", "--timeout", "1s"); out.exit != 4 || out.obj["code"] != "not_enrolled" {
		t.Fatalf("unenrolled wait: exit %d, %s", out.exit, out.stdout)
	}
}
