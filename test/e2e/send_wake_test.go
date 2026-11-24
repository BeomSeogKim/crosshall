//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A sender is told, per recipient, what will tell it about the request: a
// Claude Code and a Codex CLI member joined by their hooks, an Antigravity
// CLI member and a crosshall run shell joined by hand. None of them can be
// woken here, each for its own fixed reason, so one next: line names them.
func TestSendSaysWhatWillTellEachRecipient(t *testing.T) {
	f := newSetupWorld(t)
	for _, adapter := range []string{"claude", "codex"} {
		f.apply("", "setup", adapter, "--apply")
	}
	repo := f.enrollmentRepo() // the shell member @peer is in #setup-check
	db := f.db()
	for _, adapter := range []string{"claude", "codex"} {
		for deadline := time.Now().Add(fixtureEventTimeout); ; {
			f.invokeHook(repo, adapter, "SessionStart", `"source":"startup"`)
			f.waitEnrollmentPackets(adapter, "SessionStart", deadline)
			var live bool
			if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM binding b JOIN principal_key k ON k.principal_id = b.principal_id
				WHERE k.vendor = ? AND k.key = ? AND b.state = 'live')`, adapter, setupSessionPrefix+adapter).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if live {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not join: %s", adapter, f.enrollmentEvidence(adapter))
			}
		}
	}
	agy := &participant{w: f.world, extra: []string{"ANTIGRAVITY_CONVERSATION_ID=send-wake-agy"}}
	agy.must("join", "setup-check", "--as", "agy")
	sender := f.member()
	sender.must("join", "setup-check", "--as", "sender")

	type wake struct {
		Alias   string `json:"alias"`
		State   string `json:"state"`
		Next    string `json:"next"`
		Wake    string `json:"wake"`
		Why     string `json:"why"`
		Adapter string `json:"adapter"`
		SeenAt  string `json:"seen_at"`
	}
	sent := sender.must("ask", "@claude1", "@codex1", "@agy", "@peer", "--new", "review this")
	var result struct {
		Recipients []wake `json:"recipients"`
	}
	if err := json.Unmarshal([]byte(sent.stdout), &result); err != nil {
		t.Fatalf("ask JSON: %v\n%s", err, sent.stdout)
	}
	// The fake Claude Code session has no message socket, so its route is
	// invalidated, and Codex CLI has no sender: neither has a notice route,
	// and both offer pending work at a prompt.
	const pull = "waits for its next crosshall command"
	want := map[string]wake{
		"claude1": {Alias: "claude1", State: "stored", Next: pull, Wake: "none", Why: "no_route", SeenAt: "prompt"},
		"codex1":  {Alias: "codex1", State: "stored", Next: pull, Wake: "none", Why: "no_route", Adapter: "codex", SeenAt: "prompt"},
		"agy":     {Alias: "agy", State: "stored", Next: pull, Wake: "none", Why: "no_route", SeenAt: "command"},
		"peer":    {Alias: "peer", State: "stored", Next: pull, Wake: "none", Why: "no_route", SeenAt: "command"},
	}
	if len(result.Recipients) != len(want) {
		t.Fatalf("recipients %+v", result.Recipients)
	}
	for _, got := range result.Recipients {
		if got != want[got.Alias] {
			t.Errorf("@%s: %+v, want %+v", got.Alias, got, want[got.Alias])
		}
	}

	text := sender.mustPlain("ask", "@agy", "@peer", "a second look")
	for _, line := range []string{
		"  @agy   stored -> not woken: no notice route; it sees this at its next crosshall command\n",
		"  @peer  stored -> not woken: no notice route; it sees this at its next crosshall command\n",
		"next: nothing will wake @agy or @peer. Ask a member the hall can wake (crosshall who), or tell your user.\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("send text lacks %q:\n%s", line, text)
		}
	}
	waiting := sender.mustPlain("waiting")
	if !strings.Contains(waiting, "@codex1 stored -> not woken: no notice route for codex; it sees this at its next prompt\n") {
		t.Errorf("waiting:\n%s", waiting)
	}
	who := sender.mustPlain("who")
	if !strings.Contains(who, "not woken: no notice route for codex\n") {
		t.Errorf("who lacks the Codex row:\n%s", who)
	}
	var claudeRow string
	for line := range strings.Lines(who) {
		if strings.Contains(line, "@claude1 ") {
			claudeRow = line
		}
	}
	if !strings.HasSuffix(claudeRow, "not woken: no notice route\n") {
		t.Errorf("who's @claude1 row %q, want no notice route:\n%s", claudeRow, who)
	}
}
