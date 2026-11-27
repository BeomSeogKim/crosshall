//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/publish"
)

func TestSpokeLinkWakesClaudeThroughNativeInbox(t *testing.T) {
	const sessionID = "linked-claude-native-inbox"
	const version = "2.1.9262"
	f := newLinkFixture(t, binary, binary)
	t.Cleanup(func() {
		if t.Failed() {
			f.home.logDispatcher()
			f.spoke.logDispatcher()
		}
	})
	tmp, err := os.MkdirTemp("/tmp", "ch-linked-wake-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	tmp, err = filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	sockDir := filepath.Join(tmp, "cc-socks")
	if err := os.Mkdir(sockDir, 0700); err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	socket := filepath.Join(sockDir, pid+".sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	lines := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(fixtureEventTimeout))
			line, err := bufio.NewReader(conn).ReadString('\n')
			_ = conn.Close()
			if err != nil {
				continue
			}
			select {
			case lines <- line:
			case <-t.Context().Done():
				return
			}
		}
	}()

	f.spoke.extraEnv = append(f.spoke.extraEnv, "XDG_RUNTIME_DIR="+tmp)
	home, spoke := f.home.owner(), f.spoke.owner()
	home.must("owner", "start")
	spoke.must("owner", "start")
	home.must("owner", "channel", "create", "native-wake")
	home.must("owner", "adapter", "set", "claude", "--enabled", "on")
	home.must("owner", "publish", "hall", "on")

	var revision string
	for _, spec := range hook.InstallSpecs() {
		if spec.Adapter == "claude" {
			revision = spec.Revision
			break
		}
	}
	if revision == "" {
		t.Fatal("Claude hook manifest missing from the fixture binary")
	}
	evidence, err := json.Marshal(domain.AdapterEvidence{
		Vendor: "claude", VendorVersion: version, Mode: "interactive", ManifestRev: revision,
		Platform: runtime.GOOS + "-" + runtime.GOARCH, RunAtMS: time.Now().UnixMilli(),
		Results: []domain.EvidenceResult{{Cap: "publish", Level: "tested", Check: "fixture inbox for the linked native sender"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(f.home.home, "claude-native-wake-evidence.json")
	if err := os.WriteFile(evidencePath, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	home.must("owner", "adapter", "evidence", evidencePath)

	sender := f.home.member()
	sender.must("join", "native-wake", "--as", "sender")
	spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	waitSpokeLink(t, spoke, "connected")
	receiver := &participant{w: f.spoke, extra: []string{"CLAUDE_CODE_SESSION_ID=" + sessionID, "CLAUDE_PID=" + pid}}
	receiver.must("join", "native-wake", "--as", "receiver")

	homeDB := f.home.db()
	spokeDB := spokeStore(t, f.spoke.state)
	eventually(t, "the experimental link negotiated wake support and cached the Claude policy", func() bool {
		var wake, enabled bool
		return homeDB.QueryRow(`SELECT wake_capable FROM link_event_stream WHERE closed_at_ms IS NULL`).Scan(&wake) == nil && wake &&
			spokeDB.QueryRow(`SELECT c.eligible AND p.enabled FROM spoke_event_cursor c, spoke_policy p
				WHERE p.adapter = 'claude'`).Scan(&enabled) == nil && enabled
	})
	repo := filepath.Join(f.spoke.home, "checkout")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	start, err := json.Marshal(map[string]any{
		"hook_event_name": "SessionStart", "session_id": sessionID, "cwd": repo,
		"source": "startup", "permission_mode": "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), f.spoke.binaryPath(), "hook", "claude", "SessionStart")
	cmd.Dir, cmd.Stdin = repo, bytes.NewReader(start)
	cmd.Env = append(f.spoke.baseEnv(), receiver.extra...)
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_MESSAGING_SOCKET="+socket,
		"AI_AGENT=claude-code_"+strings.ReplaceAll(version, ".", "-")+"_harness")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("linked Claude start: %v: %s", err, out)
	}
	eventually(t, "the home received the verified launch and matching scope cut", func() bool {
		var ready bool
		return homeDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM link_event_launch l JOIN link_event_cut c
			ON c.device_id = l.device_id AND c.binding_id = l.binding_id AND c.boot = l.boot
			AND c.leaf_generation = l.leaf_generation AND c.membership_generation = l.membership_generation
			AND c.route_epoch = l.route_epoch
			WHERE c.vendor = 'claude' AND c.key = ? AND c.absent = 0 AND l.verified = 1)`, sessionID).
			Scan(&ready) == nil && ready
	})

	const request = "Check the linked native inbox."
	askID := num(t, sender.must("ask", "@receiver", request), "id")
	adapter, ok := publish.Lookup("claude")
	if !ok {
		t.Fatal("Claude publish adapter missing from the fixture binary")
	}
	wantNotice, ok := adapter.Notice([]int64{askID})
	if !ok {
		t.Fatal("Claude did not format the wake notice")
	}
	select {
	case line := <-lines:
		var msg struct {
			Type     string
			From     string
			Priority string
			Message  struct{ Content string }
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.Type != "user" ||
			msg.From != "crosshall" || msg.Priority != "later" || msg.Message.Content != wantNotice {
			t.Fatalf("native inbox got %q (%v), wanted one later notice %q", line, err, wantNotice)
		}
	case <-time.After(fixtureEventTimeout):
		t.Fatalf("native inbox did not receive the wake for request %d", askID)
	}

	var attempt, cut int64
	eventually(t, "the home and spoke recorded the same submitted wake and leaf cut", func() bool {
		if err := homeDB.QueryRow(`SELECT a.id, a.submission_seq FROM link_wake_attempt a
			WHERE a.vendor = 'claude' AND a.state = 'submitted' AND a.submission_seq > 0
			AND a.released_at_ms IS NULL AND EXISTS (SELECT 1 FROM json_each(a.message_ids) WHERE value = ?)`, askID).
			Scan(&attempt, &cut); err != nil {
			return false
		}
		var stored bool
		return spokeDB.QueryRow(`SELECT state = 'finished' AND outcome = 'submitted' AND recorded = 1 AND submission_seq = ?
			FROM spoke_wake_attempt WHERE attempt_id = ?`, cut, attempt).Scan(&stored) == nil && stored
	})
	if got := receiver.must("read", strconv.FormatInt(askID, 10)); !strings.Contains(got.stdout, request) {
		t.Fatalf("the linked Claude receiver could not read its request: %s", got.stdout)
	}
	eventually(t, "the spoke command pickup released the wake at a later leaf cut", func() bool {
		var released bool
		return homeDB.QueryRow(`SELECT released_at_ms IS NOT NULL AND activity_seq > submission_seq
			FROM link_wake_attempt WHERE id = ?`, attempt).Scan(&released) == nil && released
	})
	select {
	case line := <-lines:
		t.Fatalf("native inbox sent a duplicate wake: %s", line)
	default:
	}

	// The device submitted a wake on this route: the home now reports it
	// as a notice route, and the next request is woken the same way (#438).
	for _, m := range sender.must("who").obj["members"].([]any) {
		if row := m.(map[string]any); row["alias"] == "receiver" && row["wake"] != "notice" {
			t.Fatalf("who reports the linked receiver as %v", row)
		}
	}
	asked := sender.must("ask", "@receiver", "Check the linked native inbox again.")
	if recipients, _ := asked.obj["recipients"].([]any); len(recipients) != 1 ||
		recipients[0].(map[string]any)["wake"] != "notice" {
		t.Fatalf("the second ask did not report a notice for the linked receiver: %v", asked.obj)
	}
	select {
	case <-lines:
	case <-time.After(fixtureEventTimeout):
		t.Fatalf("native inbox did not receive the wake for request %d", num(t, asked, "id"))
	}
}
