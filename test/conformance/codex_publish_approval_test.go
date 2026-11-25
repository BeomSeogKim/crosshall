//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func runCodexPublishApproval(t *testing.T, f *nativeCrossVendorFixture) {
	t.Helper()
	const body = "Codex publish trial request during owner approval"
	model := f.codexPeer.member.model
	model.begin("", []crossVendorStep{{command: "/usr/bin/true", requiresApproval: true}})
	turn := f.codex.startApprovalTurn(t)
	waitForCodexApprovalFact(t, f, turn)
	f.codex.assertApprovalPending(t)

	ask := f.commands.ask("codex", body)
	ask.wants = append(ask.wants, "not woken: its turn waits on its owner's approval")
	claude := f.claudePeer
	claude.member.model.begin("", []crossVendorStep{ask})
	claude.sawTurn(t, claude.turn(t, crossVendorPrompt))
	claude.member.model.verify(t)
	askID, err := crossVendorMessageID(t.Context(), f.db, "ask", body)
	codexMust(t, err)

	// Keep the approval open through several dispatcher ticks without answering it.
	time.Sleep(time.Second)
	f.codex.assertApprovalPending(t)
	var recipient, attempts int
	codexMust(t, f.db.QueryRowContext(t.Context(), `SELECT
		(SELECT COUNT(*) FROM recipient r JOIN principal_key k ON k.principal_id = r.principal_id
		 WHERE r.message_id = ? AND r.picked_up_at_ms IS NULL AND k.vendor = 'codex' AND k.key = ?),
		(SELECT COUNT(*) FROM publish_attempt_item WHERE message_id = ?)`, askID, f.codex.id, askID).Scan(&recipient, &attempts))
	if recipient != 1 || attempts != 0 {
		t.Fatalf("approval ask #%d: want one unread recipient and zero publish attempts, got %d and %d", askID, recipient, attempts)
	}
	model.mu.Lock()
	requests, failure := model.request, model.failure
	model.mu.Unlock()
	if requests != 1 || failure != nil {
		t.Fatalf("approval turn should have made only the approval request: got %d model requests, %v", requests, failure)
	}
	t.Logf("approval confirmed for Codex turn %s; ask #%d remained unread with no publish attempt, queue entry or wake turn", turn, askID)
}

func (s *nativeCodexSession) startApprovalTurn(t *testing.T) string {
	t.Helper()
	codexMust(t, s.client.ConformanceDeadline(time.Now().Add(15*time.Second)))
	request, err := s.client.ConformanceStart("turn/start", map[string]any{
		"threadId":       s.id,
		"input":          []map[string]string{{"type": "text", "text": "Request the fixture command's approval."}},
		"approvalPolicy": "on-request",
		"sandboxPolicy":  map[string]string{"type": "workspaceWrite"},
	})
	codexMust(t, err)
	var started, pending string
	for started == "" || pending == "" {
		event := s.read(t)
		if event.Method == "" && event.ID == request {
			started = event.Result.Turn.ID
			if started == "" {
				t.Fatal("Codex start did not acknowledge an approval probe turn")
			}
		}
		if event.Params.ThreadID != s.id {
			continue
		}
		switch event.Method {
		case "item/commandExecution/requestApproval":
			pending = event.Params.TurnID
			if pending == "" {
				t.Fatal("Codex approval request had no turn ID")
			}
		case "turn/completed":
			t.Fatalf("Codex approval probe completed without an owner decision: %+v", event.Params.Turn)
		}
	}
	if started != pending {
		t.Fatalf("Codex started turn %s, but approval was for turn %s", started, pending)
	}
	return pending
}

func waitForCodexApprovalFact(t *testing.T, f *nativeCrossVendorFixture, turn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var hook, confirmation int
	for {
		err := f.db.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(o.source <> ?), 0), COALESCE(SUM(o.source = ?), 0)
			FROM observation o JOIN principal_key k ON k.principal_id = o.principal_id
			WHERE k.vendor = 'codex' AND k.key = ? AND o.kind = 'approval_requested' AND o.turn_ref = ?`,
			store.ApprovalShownSource, store.ApprovalShownSource, f.codex.id, turn).Scan(&hook, &confirmation)
		if err == nil && hook > 0 && confirmation > 0 {
			return
		}
		if err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("Codex approval turn %s: want both PermissionRequest and app-server confirmation, got %d hook and %d confirmation observations", turn, hook, confirmation)
		}
	}
}

func (s *nativeCodexSession) assertApprovalPending(t *testing.T) {
	t.Helper()
	codexMust(t, s.client.ConformanceDeadline(time.Now().Add(10*time.Second)))
	var result struct {
		Thread struct {
			Status struct {
				Type        string
				ActiveFlags []string
			}
		}
	}
	params := map[string]string{"threadId": s.id}
	codexMust(t, s.client.Call("thread/read", params, &result))
	status := result.Thread.Status
	if status.Type != "active" || !slices.Contains(status.ActiveFlags, "waitingOnApproval") {
		t.Fatalf("Codex thread did not remain in native approval: %+v", status)
	}
	var queue struct{ Data []json.RawMessage }
	codexMust(t, s.client.Call("thread/queue/list", params, &queue))
	if queue.Data == nil || len(queue.Data) != 0 {
		t.Fatalf("Codex approval combined with unexpected native queue data: %s", queue.Data)
	}
}
