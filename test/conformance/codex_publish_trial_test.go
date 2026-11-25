//go:build conformance

package conformance

import (
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestCodexDaemonPublishTrial(t *testing.T) {
	f := newNativeCrossVendorFixture(t, "0.172.0")
	if revision := nativeFixtureManifestRevision(t, "codex"); revision != "codex-1" {
		t.Fatalf("publish trial needs codex-1, found %s", revision)
	}
	wantPlatform := runtime.GOOS + "-" + runtime.GOARCH
	var version, platform, mode, revision string
	codexMust(t, f.db.QueryRowContext(t.Context(), `SELECT json_extract(l.profile, '$.vendor_version'),
		json_extract(l.profile, '$.platform'), json_extract(l.profile, '$.mode'), json_extract(l.profile, '$.manifest_rev')
		FROM adapter_launch l JOIN principal_key k ON k.principal_id = l.principal_id
		WHERE k.vendor = 'codex' AND k.key = ? AND l.ended_hook_at_ms IS NULL
		ORDER BY l.started_hook_at_ms DESC, l.rowid DESC LIMIT 1`, f.codex.id).Scan(&version, &platform, &mode, &revision))
	if version != "0.172.0" || platform != wantPlatform || mode != "daemon" || revision != "codex-1" {
		t.Fatalf("unexpected observed Codex profile: %s %s %s %s", version, platform, mode, revision)
	}
	if !t.Run("queued_read_and_settled_once", func(t *testing.T) {
		before := codexPublishThread(t, f.codex)
		id := runNativeCrossVendorRoundTrip(t, f.db, f.commands, f.claudePeer, f.codexPeer)
		want := append(before, f.codexPeer.lastTurn)
		// Observe several dispatcher ticks after the receipt frees the lane.
		for quietUntil := time.Now().Add(2 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if turns := codexPublishThread(t, f.codex); !slices.Equal(turns, want) {
				t.Fatalf("Codex ask #%d: thread must complete exactly one new turn, got %v, want %v", id, turns, want)
			}
			assertCodexPublishQueueEmpty(t, f.codex)
			if time.Now().After(quietUntil) {
				break
			}
		}
		f.codexPeer.member.model.verify(t)
	}) {
		return
	}
	if !t.Run("server_approval_stops_publish", func(t *testing.T) { runCodexPublishApproval(t, f) }) {
		return
	}
	requests := func(m *crossVendorModel) int {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.sequence
	}
	t.Logf("publish trial passed: codex %s %s %s %s checked_at_ms=%d evidence_source=%s paid_model_turns=0 codex_fixture_requests=%d claude_fixture_requests=%d",
		version, platform, mode, revision, time.Now().UnixMilli(), f.codexEvidenceSource, requests(f.codexPeer.member.model), requests(f.claudePeer.member.model))
}

func codexPublishThread(t *testing.T, s *nativeCodexSession) []string {
	t.Helper()
	codexMust(t, s.client.ConformanceDeadline(time.Now().Add(10*time.Second)))
	var snapshot struct {
		Thread struct {
			Status struct{ Type string }
			Turns  []struct{ ID, Status string }
		}
	}
	codexMust(t, s.client.Call("thread/read", map[string]any{"threadId": s.id, "includeTurns": true}, &snapshot))
	if snapshot.Thread.Status.Type != "idle" {
		t.Fatalf("Codex thread %s is %q, want idle", s.id, snapshot.Thread.Status.Type)
	}
	ids := make([]string, 0, len(snapshot.Thread.Turns))
	for _, turn := range snapshot.Thread.Turns {
		if turn.ID == "" || turn.Status != "completed" {
			t.Fatalf("unexpected turn in Codex thread %s: %+v", s.id, turn)
		}
		ids = append(ids, turn.ID)
	}
	return ids
}

func assertCodexPublishQueueEmpty(t *testing.T, s *nativeCodexSession) {
	t.Helper()
	codexMust(t, s.client.ConformanceDeadline(time.Now().Add(10*time.Second)))
	var page struct {
		Data       []struct{ ID string }
		NextCursor string
	}
	codexMust(t, s.client.Call("thread/queue/list", map[string]string{"threadId": s.id}, &page))
	if len(page.Data) != 0 || page.NextCursor != "" {
		t.Fatalf("Codex thread %s has unexpected queued notices: %v", s.id, page.Data)
	}
}
