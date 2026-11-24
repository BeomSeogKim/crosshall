//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

func TestManifestEvidenceCoversAnotherBuildsPublishAndStop(t *testing.T) {
	const evidenceVersion, launchVersion = "2.1.9260", "2.1.9261"
	const manifest, mode = "claude-3", "interactive"
	platform := runtime.GOOS + "-" + runtime.GOARCH
	tmp, err := os.MkdirTemp("/tmp", "ch-evidence-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	socketDir := filepath.Join(tmp, "cc-socks")
	if err := os.Mkdir(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketDir, "fixture.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	w := newWorld(t)
	owner := w.owner()
	owner.extra = []string{"TMPDIR=" + tmp}
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	repo := filepath.Join(w.home, "checkout")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	owner.must("owner", "adapter", "set", "claude", "--enabled", "on")
	writer := w.member()
	writer.must("join", "checkout", "--as", "writer")
	db := w.db()
	committed := func(query string, args ...any) bool {
		t.Helper()
		var found bool
		if err := db.QueryRow(query, args...).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}

	importEvidence := func(version, cap, level string, at int64) {
		t.Helper()
		data, err := json.Marshal(domain.AdapterEvidence{Vendor: "claude", VendorVersion: version,
			Platform: platform, Mode: mode, ManifestRev: manifest, RunAtMS: at,
			Results: []domain.EvidenceResult{{Cap: cap, Level: level, Check: "synthetic fixture evidence"}}})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(w.home, "evidence.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if got := num(t, owner.must("owner", "adapter", "evidence", path), "rows"); got != 1 {
			t.Fatalf("recording %s evidence for %s changed %d rows, want 1", cap, version, got)
		}
	}
	var shipped []struct {
		version string
		cap     string
		at      int64
	}
	rows, err := db.Query(`SELECT vendor_version, cap, checked_at_ms FROM capability_evidence
		WHERE vendor = 'claude' AND platform = ? AND mode = ? AND manifest_rev = ? AND cap IN ('publish', 'turn_end') AND level = 'tested'`,
		platform, mode, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var item struct {
			version string
			cap     string
			at      int64
		}
		if err := rows.Scan(&item.version, &item.cap, &item.at); err != nil {
			t.Fatal(err)
		}
		if item.version == evidenceVersion || item.version == launchVersion {
			t.Fatalf("synthetic version %s overlaps shipped evidence", item.version)
		}
		shipped = append(shipped, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, item := range shipped {
		importEvidence(item.version, item.cap, "candidate", item.at+1)
	}
	if committed(`SELECT EXISTS(SELECT 1 FROM capability_evidence WHERE vendor = 'claude' AND platform = ? AND mode = ?
		AND manifest_rev = ? AND cap IN ('publish', 'turn_end') AND level = 'tested')`, platform, mode, manifest) {
		t.Fatal("fixture started with tested evidence for its profile family")
	}
	importEvidence(evidenceVersion, "turn_end", "tested", time.Now().UnixMilli())

	c := claudeSession{w: w, key: "manifest-evidence-fixture", pid: strconv.Itoa(os.Getpid()), repo: repo}
	fire := func(event string, fields map[string]any) string {
		t.Helper()
		fields["hook_event_name"], fields["session_id"], fields["cwd"] = event, c.key, repo
		payload, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), binary, "hook", "claude", event)
		cmd.Dir, cmd.Stdin = repo, strings.NewReader(string(payload))
		cmd.Env = append(w.baseEnv(), "TMPDIR="+tmp, "CLAUDE_CODE_SESSION_ID="+c.key,
			"CLAUDE_PID="+c.pid, "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_MESSAGING_SOCKET="+socket,
			"AI_AGENT=claude-code_"+strings.ReplaceAll(launchVersion, ".", "-")+"_harness")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s hook: %v: %s", event, err, out)
		}
		return string(out)
	}
	waitForHook := func(event string, fields map[string]any, found func() bool) string {
		t.Helper()
		deadline := time.Now().Add(fixtureEventTimeout)
		for time.Now().Before(deadline) {
			out := fire(event, fields)
			c.waitForConsumption(deadline)
			if found() {
				return out
			}
		}
		t.Fatalf("%s did not commit; last hook counter %q", event, c.lastCounter())
		return ""
	}
	waitForHook("SessionStart", map[string]any{"source": "startup", "permission_mode": "default"}, func() bool {
		return committed(`SELECT EXISTS(SELECT 1 FROM publish_route r JOIN adapter_launch l
			ON l.principal_id = r.principal_id AND l.launch_ref = r.launch_ref
			WHERE r.session_ref = ? AND r.socket_path = ? AND l.verified = 1
			AND json_extract(l.profile, '$.vendor_version') = ? AND json_extract(l.profile, '$.manifest_rev') = ?)`,
			c.key, socket, launchVersion, manifest)
	})
	turn := map[string]any{"prompt_id": "fixture-turn"}
	waitForHook("UserPromptSubmit", turn, func() bool {
		return committed(`SELECT EXISTS(SELECT 1 FROM turn WHERE turn_ref = 'fixture-turn')`)
	})
	stopID := num(t, writer.must("ask", "@claude1", "Fixture work during a turn"), "id")
	turn["stop_hook_active"] = false
	stopOut := waitForHook("Stop", turn, func() bool {
		return committed(`SELECT EXISTS(SELECT 1 FROM announcement WHERE message_id = ? AND via = 'turn_end')`, stopID)
	})
	if got, want := hostContext(t, stopOut), fmt.Sprintf("[crosshall] pending: %d", stopID); got != "" && got != want {
		t.Fatalf("Stop context = %q, want %q", got, want)
	}

	// #261: a Stop waits for the dispatcher only while an ask is left for a
	// turn-end notice to announce; a note never continues a turn.
	claude := &participant{w: w, extra: []string{"TMPDIR=" + tmp, "CLAUDE_CODE_SESSION_ID=" + c.key}}
	turnStops := func(turnRef string, during func() string, wantOutcome string) {
		t.Helper()
		fields := map[string]any{"prompt_id": turnRef}
		waitForHook("UserPromptSubmit", fields, func() bool {
			return committed(`SELECT EXISTS(SELECT 1 FROM turn WHERE turn_ref = ?)`, turnRef)
		})
		wantContext := during()
		fields["stop_hook_active"] = false
		out := fire("Stop", fields)
		c.waitForConsumption(time.Now().Add(fixtureEventTimeout))
		if got := hostContext(t, out); got != wantContext || c.lastCounter() != "Stop "+wantOutcome {
			t.Fatalf("Stop of %s: context %q, counter %q; want %q, Stop %s", turnRef, got, c.lastCounter(), wantContext, wantOutcome)
		}
	}
	quiet := func() string { return "" }
	turnStops("announced", quiet, "clean")
	var note, asked int64
	turnStops("noted", func() string {
		note = num(t, writer.must("note", "@claude1", "Fixture note during a turn"), "id")
		return ""
	}, "clean")
	turnStops("asked", func() string {
		asked = num(t, writer.must("ask", "@claude1", "Fixture ask during a turn"), "id")
		return fmt.Sprintf("[crosshall] pending: %d", asked)
	}, "rpc")
	claude.must("read", strconv.FormatInt(note, 10), strconv.FormatInt(asked, 10))
	claude.must("reply", strconv.FormatInt(asked, 10), "done")
	// An ask read before its turn ends leaves the Stop waiting for nothing;
	// the next hook lowers the record, though the ask is still owed.
	var picked string
	turnStops("picked-up", func() string {
		picked = strconv.FormatInt(num(t, writer.must("ask", "@claude1", "Fixture ask read during a turn"), "id"), 10)
		claude.must("read", picked)
		return ""
	}, "rpc")
	turnStops("after-reading", quiet, "clean")
	claude.must("reply", picked, "done")

	importEvidence(evidenceVersion, "publish", "tested", time.Now().UnixMilli())
	deadline := time.Now().Add(fixtureEventTimeout)
	for !committed(`SELECT EXISTS(SELECT 1 FROM publish_route_proof p JOIN publish_route r
		ON r.binding_id = p.binding_id AND r.generation = p.generation WHERE r.session_ref = ? AND p.activated_at_ms IS NOT NULL)`, c.key) {
		if time.Now().After(deadline) {
			w.logDispatcher()
			t.Fatalf("publish route stayed inactive after the version %s evidence import", evidenceVersion)
		}
		time.Sleep(25 * time.Millisecond)
	}
	// #194: an empty inbox tells the session the hall reaches that no wait
	// is needed, and the pull member beside it to wait.
	const waitLine = "Inbox is empty. Run crosshall wait --existing --work-only --timeout 9m.\n"
	claude.must("read")
	claude.must("reply", strconv.FormatInt(stopID, 10), "done")
	if out := claude.mustPlain("inbox"); !strings.HasPrefix(out, "Inbox is empty. "+domain.HallDelivers+"\n") {
		t.Fatalf("empty inbox of a session the hall reaches:\n%s", out)
	}
	writer.must("read")
	if out := writer.mustPlain("inbox"); !strings.HasPrefix(out, waitLine) {
		t.Fatalf("empty inbox of a pull member:\n%s", out)
	}
	ask := writer.must("ask", "@claude1", "Fixture work for publish")
	var result domain.AskResult
	if err := json.Unmarshal([]byte(ask.stdout), &result); err != nil || result.ID <= 0 || len(result.Recipients) != 1 || result.Recipients[0].Next != "notice queued" {
		t.Fatalf("cross-version publish was not queued: %s (%v)", ask.stdout, err)
	}
	deadline = time.Now().Add(fixtureEventTimeout)
	if err := listener.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			t.Fatalf("no publish message to version %s: %v", launchVersion, err)
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
		var notice struct {
			Message  struct{ Content string }
			Priority string
		}
		err = json.NewDecoder(conn).Decode(&notice)
		_ = conn.Close()
		if errors.Is(err, io.EOF) {
			continue
		}
		if err != nil || notice.Priority != "later" || !strings.HasPrefix(notice.Message.Content, fmt.Sprintf("[crosshall] pending: %d.", result.ID)) {
			t.Fatalf("publish message = %+v (%v), want later notice for %d", notice, err, result.ID)
		}
		break
	}
}
