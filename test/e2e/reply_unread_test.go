//go:build e2e

package e2e

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestReplyBeforeReadClearsOnlyTheSettledRequest(t *testing.T) {
	_, owner, members := setup(t)
	asker, recipient, peer := members[0], members[1], members[2]
	first := num(t, asker.must("ask", "@tax", "first request"), "id")
	if first != 1 {
		t.Fatalf("unexpected first message ID %d", first)
	}
	output := recipient.mustPlain("reply", "1", "Done")
	const readFirst = "crosshall read 1"
	if !strings.Contains(output, "To read the original request, run "+readFirst+".") || strings.Contains(output, "Run crosshall inbox.") {
		t.Errorf("reply should point to the original request: %s", output)
	}
	inbox := recipient.must("inbox")
	if len(ids(inbox.obj["unread"])) != 0 || len(ids(inbox.obj["owed"])) != 0 {
		t.Errorf("answered request remained in inbox: %s", inbox.stdout)
	}
	timeout := recipient.run("wait", "--existing", "--work-only", "--timeout", "200ms")
	if timeout.exit != 6 {
		t.Errorf("wait returned settled work: exit %d, %s", timeout.exit, timeout.stdout)
	}
	checkUnpicked := func(id int64, state string) {
		t.Helper()
		row := ledgerRecipients(t, owner.must("owner", "status", "--all"), id)["tax"]
		if row["state"] != state || row["picked_up_at_ms"] != nil || slices.Contains(stages(row), "picked_up") {
			t.Errorf("settlement changed pickup evidence: %v", row)
		}
	}
	checkUnpicked(first, "replied")

	note := num(t, asker.must("note", "@tax", "unrelated note"), "id")
	second := num(t, asker.must("ask", "@tax", "@lint", "second request"), "id")
	id := strconv.FormatInt(second, 10)
	if out := recipient.mustPlain("reply", id, "--more-time"); strings.Contains(out, "To read the original request") {
		t.Errorf("more time showed a settlement hint: %s", out)
	}
	if unread := ids(recipient.must("inbox").obj["unread"]); !slices.Equal(unread, []int64{note, second}) {
		t.Errorf("more time should preserve the unread request: %v", unread)
	}
	decline := []string{"--key", "decline-once", "reply", id, "--decline"}
	receipt := recipient.must(decline...)
	replay := recipient.must(decline...)
	if num(t, replay, "id") != num(t, receipt, "id") {
		t.Fatal("retry created another decline")
	}
	if out := recipient.mustPlain(decline...); !strings.Contains(out, "To read the original request, run crosshall read "+id+".") {
		t.Errorf("decline omitted the original request command: %s", out)
	}
	if unread := ids(recipient.must("inbox").obj["unread"]); !slices.Equal(unread, []int64{note}) {
		t.Errorf("decline should leave only the note unread: %v", unread)
	}
	if unread := ids(peer.must("inbox").obj["unread"]); !slices.Equal(unread, []int64{second}) {
		t.Errorf("decline changed a peer's unread work: %v", unread)
	}
	if got := ids(recipient.must("read").obj["messages"]); !slices.Equal(got, []int64{note}) {
		t.Errorf("default read picked up settled work: %v", got)
	}
	checkUnpicked(first, "replied")
	checkUnpicked(second, "declined")
	if text := recipient.mustPlain(strings.Fields(readFirst)[1:]...); !strings.Contains(text, "first request") {
		t.Fatalf("cannot read the answered request by ID: %s", text)
	}
	row := ledgerRecipients(t, owner.must("owner", "status", "--all"), first)["tax"]
	if row["picked_up_at_ms"] == nil || !slices.Contains(stages(row), "picked_up") {
		t.Fatal("explicit body read did not record pickup")
	}
	recipient.must("read", id)
	if out := recipient.mustPlain(decline...); strings.Contains(out, "To read the original request") {
		t.Errorf("replayed decline advertised a read the recipient already made: %s", out)
	}
	peer.must("read", id)
	if out := peer.mustPlain("reply", id, "Also done"); strings.Contains(out, "To read the original request") {
		t.Errorf("answer after pickup advertised the original request: %s", out)
	}
}
