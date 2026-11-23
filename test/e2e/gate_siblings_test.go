//go:build e2e

package e2e

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestGateSameVendorSiblings(t *testing.T) {
	w := newWorld(t)
	owner, sender := w.owner(), w.member()
	// Both Claude sessions run in w.home, without a route override.
	target := &participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + randomHex(16)}}
	sibling := &participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + randomHex(16)}}
	owner.must("owner", "channel", "create", "review")
	sender.must("join", "review", "--as", "author")
	target.must("join", "review", "--as", "editor")
	sibling.must("join", "review", "--as", "observer")

	const question = "Which edition goes to print?"
	const answer = "Use the evening edition."
	ask := []string{"ask", "@editor", question, "--key", "print-edition"}
	// Replaying the author's ask returns the current recipient state.
	delivery := func(want string) int64 {
		t.Helper()
		o := sender.must(ask...)
		var result struct {
			ID         int64
			Recipients []struct{ Alias, State string }
		}
		if err := json.Unmarshal([]byte(o.stdout), &result); err != nil {
			t.Fatal(err)
		}
		if result.ID < 1 || len(result.Recipients) != 1 || result.Recipients[0].Alias != "editor" || result.Recipients[0].State != want {
			t.Fatalf("want one delivery to editor in state %s, got %s", want, o.stdout)
		}
		return result.ID
	}
	id := strconv.FormatInt(delivery("stored"), 10)

	// Unaddressed members can browse channel history. Browsing must leave
	// the editor's copy unread, and the observer has no obligation to settle.
	if got := siblingMessages(t, sibling.must("read")); len(got) != 0 {
		t.Fatalf("observer received the editor's unread messages: %+v", got)
	}
	if got := siblingMessages(t, sibling.must("read", id)); len(got) != 1 || got[0].Body != question {
		t.Fatalf("observer's channel history: %+v", got)
	}
	for _, attempt := range []string{"Use the early edition.", "--decline", "--more-time"} {
		o := sibling.run("reply", id, attempt)
		if o.exit != 3 || o.obj["code"] != "not_obligated" {
			t.Fatalf("observer reply %q: exit %d, %s", attempt, o.exit, o.stdout)
		}
	}
	delivery("stored")

	if got := siblingMessages(t, target.must("read")); len(got) != 1 || got[0].Body != question || got[0].Sender != "author" {
		t.Fatalf("editor's unread messages: %+v", got)
	}
	delivery("picked_up")
	target.must("reply", id, answer)
	delivery("replied")
	if got := siblingMessages(t, sender.must("read")); len(got) != 1 || got[0].Body != answer || got[0].Sender != "editor" {
		t.Fatalf("author's replies: %+v", got)
	}
}

type siblingMessage struct{ Body, Sender string }

func siblingMessages(t *testing.T, o outcome) []siblingMessage {
	t.Helper()
	var result struct{ Messages []siblingMessage }
	if err := json.Unmarshal([]byte(o.stdout), &result); err != nil || result.Messages == nil {
		t.Fatalf("want a messages array, got %s (decode error %v)", o.stdout, err)
	}
	return result.Messages
}
