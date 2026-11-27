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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// A linked Codex session is woken through its managed server's queue,
// whether the hall saw its start or it joined by hand during a turn whose
// end alone named the server (#437).
// With a home, device or hook of the base build, the session is woken
// after its next prompt's turn, as before; before that prompt an ask never
// reports a wake the device holds no server route for.
func TestSpokeLinkWakesCodexThroughNativeQueue(t *testing.T) {
	t.Run("started", func(t *testing.T) { spokeLinkWakesCodex(t, binary, binary, "", []string{"SessionStart"}, nil) })
	t.Run("joined-during-a-turn", func(t *testing.T) { spokeLinkWakesCodex(t, binary, binary, "", []string{"Stop"}, nil) })
	older := os.Getenv(baseBinaryEnv)
	for _, c := range []struct{ name, home, spoke, hook string }{
		{"joined-during-a-turn/base-home", older, binary, ""},
		{"joined-during-a-turn/base-spoke", binary, older, ""},
		{"joined-during-a-turn/base-hook", binary, binary, older},
	} {
		t.Run(c.name, func(t *testing.T) {
			if older == "" {
				t.Skip("set " + baseBinaryEnv + " to a previous build with -tags=testvendorhome")
			}
			requireFixtureBuild(t, older)
			spokeLinkWakesCodex(t, c.home, c.spoke, c.hook, []string{"Stop"}, []string{"UserPromptSubmit", "Stop"})
		})
	}
}

// spokeLinkWakesCodex runs the receiver's hooks under the managed server,
// with hookBinary as the hook when it is set: before, then, when later is
// set, an ask that must not report a device wake unless the device holds a
// server route for the receiver, then later. Its sibling starts as usual.
func spokeLinkWakesCodex(t *testing.T, homeBinary, spokeBinary, hookBinary string, before, later []string) {
	platform := map[string]string{"darwin": "apple-darwin", "linux": "unknown-linux-gnu"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if platform == "" || arch == "" {
		t.Skip("requires a supported managed Codex process and Unix peer identity")
	}
	const version = "0.171.0"
	const channel = "linked-codex-wake"
	f := newLinkFixture(t, homeBinary, spokeBinary)
	t.Cleanup(func() {
		if t.Failed() {
			f.home.logDispatcher()
			f.spoke.logDispatcher()
		}
	})

	codexHome := filepath.Join(f.spoke.home, ".codex")
	fixtureBin := filepath.Join(f.spoke.home, "bin")
	if err := os.Mkdir(fixtureBin, 0700); err != nil {
		t.Fatal(err)
	}
	versionProbe := []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf '%s\\n' 'codex-cli " + version + "'\n")
	if err := os.WriteFile(filepath.Join(fixtureBin, "codex"), versionProbe, 0700); err != nil {
		t.Fatal(err)
	}
	installer := &participant{w: f.spoke, extra: []string{"PATH=" + fixtureBin + ":/usr/bin:/bin"}}
	installer.must("setup", "codex", "--apply")

	home, spoke := f.home.owner(), f.spoke.owner()
	home.must("owner", "start")
	spoke.must("owner", "start")
	home.must("owner", "channel", "create", channel)
	home.must("owner", "adapter", "set", "codex", "--enabled", "on")
	home.must("owner", "publish", "hall", "on")
	evidence, err := json.Marshal(domain.AdapterEvidence{
		Vendor: "codex", VendorVersion: version, Mode: "daemon", ManifestRev: "codex-1",
		Platform: runtime.GOOS + "-" + runtime.GOARCH, RunAtMS: time.Now().UnixMilli(),
		Results: []domain.EvidenceResult{{Cap: "publish", Level: "tested", Check: "fixture managed queue for the linked native sender"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(f.home.home, "codex-wake-evidence.json")
	if err := os.WriteFile(evidencePath, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	home.must("owner", "adapter", "evidence", evidencePath)
	sender := f.home.member()
	sender.must("join", channel, "--as", "sender")
	spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	waitSpokeLink(t, spoke, "connected")
	homeDB := f.home.db()
	spokeDB := spokeStore(t, f.spoke.state)
	eventually(t, "the linked devices negotiated wake and cached the Codex policy", func() bool {
		var enabled, negotiated bool
		return homeDB.QueryRow(`SELECT wake_capable FROM link_event_stream WHERE closed_at_ms IS NULL`).Scan(&negotiated) == nil && negotiated &&
			spokeDB.QueryRow(`SELECT eligible AND enabled FROM spoke_event_cursor, spoke_policy WHERE adapter = 'codex'`).Scan(&enabled) == nil && enabled
	})

	daemon := filepath.Join(codexHome, "packages", "app-server-daemon", "releases", version+"-"+arch+"-"+platform, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(daemon), 0700); err != nil {
		t.Fatal(err)
	}
	buildCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if raw, err := exec.CommandContext(buildCtx, "go", "test", "-c", "-o", daemon, "./testdata/linked_codex_daemon").CombinedOutput(); err != nil {
		t.Fatalf("build managed Codex fixture: %v\n%s", err, raw)
	}
	tmp, err := os.MkdirTemp("/tmp", "ch-linked-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	socket := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	requests := filepath.Join(tmp, "requests.jsonl")
	hook := f.spoke.binaryPath()
	if hookBinary != "" {
		hook = hookBinary
	}
	cmd := exec.CommandContext(t.Context(), daemon, "-test.run=^TestManagedCodexDaemon$", "app-server", "--managed-daemon")
	cmd.Dir = f.spoke.home
	cmd.Env = append(f.spoke.baseEnv(), "TMPDIR="+tmp,
		"CROSSHALL_FIXTURE_CODEX_HOOK="+hook,
		"CROSSHALL_FIXTURE_CODEX_CONTROL="+socket,
		"CROSSHALL_FIXTURE_CODEX_REQUESTS="+requests)
	logPath := filepath.Join(tmp, "daemon.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case <-wait:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-wait
		}
		if t.Failed() {
			out, _ := os.ReadFile(logPath)
			t.Logf("managed Codex fixture: %s", out)
		}
	})
	eventually(t, "the managed Codex process published its control socket", func() bool {
		_, err := os.Stat(socket)
		return err == nil
	})

	threads := []struct{ id, alias string }{
		{"linked-codex-target", "receiver"}, {"linked-codex-sibling", "sibling"},
	}
	turn := 0
	send := func(thread string, events []string) {
		t.Helper()
		for _, event := range events {
			hook := map[string]any{"hook_event_name": event, "session_id": thread, "cwd": f.spoke.home}
			switch event {
			case "SessionStart":
				hook["source"] = "startup"
			case "UserPromptSubmit":
				turn++
				hook["turn_id"], hook["prompt"] = "turn-"+strconv.Itoa(turn), "fixture private text"
			case "Stop":
				// Before any prompt: the turn the session joined during ends.
				hook["turn_id"], hook["stop_hook_active"] = "turn-"+strconv.Itoa(turn), false
			}
			if err := json.NewEncoder(input).Encode(hook); err != nil {
				t.Fatal(err)
			}
		}
	}
	participants := make([]*participant, 0, len(threads))
	for _, thread := range threads {
		member := &participant{w: f.spoke, extra: []string{"CODEX_THREAD_ID=" + thread.id}}
		member.must("join", channel, "--as", thread.alias)
		participants = append(participants, member)
		events := []string{"SessionStart"}
		if thread.alias == "receiver" {
			events = before
		}
		send(thread.id, events)
	}
	if len(later) > 0 {
		// A fact held until its scope's cut arrives is applied later, and a
		// launch it opens marks the device's offers until the device
		// confirms the change: until then every ask says no_route.
		eventually(t, "the home applied the receiver's turn end and the device confirmed its offers", func() bool {
			var n, dirty int
			return homeDB.QueryRow(`SELECT COUNT(*) FROM link_event_input WHERE vendor = 'codex' AND key = ?
				AND json_extract(payload, '$.body.fact') = 'turn_finishing' AND status != 'quarantined'`, threads[0].id).Scan(&n) == nil && n == 1 &&
				homeDB.QueryRow(`SELECT COUNT(*) FROM device_offer_dirty WHERE vendor = 'codex' AND key = ?`, threads[0].id).Scan(&dirty) == nil && dirty == 0
		})
		var server int64
		if err := spokeDB.QueryRow(`SELECT COALESCE(json_extract(evidence, '$.Material.Route.Host.PID'), 0)
			FROM spoke_native_route WHERE vendor = 'codex' AND key = ?`, threads[0].id).Scan(&server); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		early := sender.must("ask", "@receiver", "Before the prompt.")
		var reply struct {
			Recipients []struct{ Alias, Wake, Next string }
		}
		if err := json.Unmarshal([]byte(early.stdout), &reply); err != nil || len(reply.Recipients) != 1 ||
			server == 0 && (reply.Recipients[0].Wake == "device" || strings.Contains(reply.Recipients[0].Next, "linked device")) {
			t.Fatalf("before its prompt the ask reports a wake the device holds no server route for (%d): %s (%v)", server, early.stdout, err)
		}
		participants[0].must("read", strconv.FormatInt(num(t, early, "id"), 10))
		send(threads[0].id, later)
	}
	eventually(t, "the home observed managed Codex launches and scope cuts for both threads", func() bool {
		var n int
		return homeDB.QueryRow(`SELECT COUNT(*) FROM link_event_launch l
			JOIN link_event_cut c ON c.device_id = l.device_id AND c.binding_id = l.binding_id AND c.boot = l.boot
			 AND c.route_epoch = l.route_epoch AND c.leaf_generation = l.leaf_generation
			 AND c.membership_generation = l.membership_generation
			JOIN adapter_launch a ON a.principal_id = l.principal_id AND a.launch_ref = l.projection_ref
			WHERE c.vendor = 'codex' AND c.key IN (?, ?) AND c.absent = 0 AND json_extract(a.profile, '$.mode') = 'daemon'`,
			threads[0].id, threads[1].id).Scan(&n) == nil && n == 2
	})

	const body = "Check the linked Codex queue."
	messageID := num(t, sender.must("ask", "@receiver", body), "id")
	var attempt, submission int64
	eventually(t, "the matching Codex wake and submission cut reached both ledgers", func() bool {
		if err := homeDB.QueryRow(`SELECT a.id, a.submission_seq FROM link_wake_attempt a
			WHERE a.state = 'submitted' AND a.released_at_ms IS NULL AND a.submission_seq > 0
			AND a.vendor = 'codex' AND json_extract(a.route, '$.key') = ?
			AND EXISTS (SELECT 1 FROM json_each(a.message_ids) WHERE value = ?)`, threads[0].id, messageID).
			Scan(&attempt, &submission); err != nil {
			return false
		}
		var recorded bool
		return spokeDB.QueryRow(`SELECT recorded AND state = 'finished' AND outcome = 'submitted' AND submission_seq = ?
			FROM spoke_wake_attempt WHERE attempt_id = ?`, submission, attempt).Scan(&recorded) == nil && recorded
	})
	receiver := participants[0]
	if got := receiver.must("read", strconv.FormatInt(messageID, 10)); !strings.Contains(got.stdout, body) {
		t.Fatalf("linked Codex could not read its request: %s", got.stdout)
	}
	eventually(t, "the later command pickup released this Codex wake", func() bool {
		var released bool
		return homeDB.QueryRow(`SELECT released_at_ms IS NOT NULL AND activity_seq > submission_seq FROM link_wake_attempt WHERE id = ?`,
			attempt).Scan(&released) == nil && released
	})

	adapter, ok := publish.Lookup("codex")
	if !ok {
		t.Fatal("Codex publish adapter missing from the fixture binary")
	}
	want, ok := adapter.Notice([]int64{messageID})
	if !ok {
		t.Fatal("Codex did not format the notice")
	}
	raw, err := os.ReadFile(requests)
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var call struct {
			Method string
			Params struct {
				ThreadID, ClientUserMessageID string
				Input                         []struct{ Type, Text string }
			}
		}
		if err := json.Unmarshal(line, &call); err != nil {
			t.Fatal(err)
		}
		switch call.Method {
		case "thread/read":
		case "thread/queue/add":
			queued++
			p := call.Params
			if p.ThreadID != threads[0].id || p.ClientUserMessageID == "" || len(p.Input) != 1 ||
				p.Input[0].Type != "text" || p.Input[0].Text != want {
				t.Fatalf("Codex queue received %s, wanted one notice for %s", line, threads[0].id)
			}
		default:
			t.Fatalf("unexpected native queue operation: %s", line)
		}
	}
	if queued != 1 {
		t.Fatalf("recorded %d native Codex submissions, wanted one on the addressed thread", queued)
	}
}
