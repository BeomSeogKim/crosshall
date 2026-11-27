//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// A Codex session whose start the hall never saw joins by hand during a
// turn. With no prompt after the join, its turn end names the managed
// server, which registers the route its start would have, and a following
// ask is queued on its thread (#437).
func TestALocalCodexHandJoinIsWokenAfterItsTurnEndAlone(t *testing.T) {
	platform := map[string]string{"darwin": "apple-darwin", "linux": "unknown-linux-gnu"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if platform == "" || arch == "" {
		t.Skip("requires a supported managed Codex process and Unix peer identity")
	}
	const version = "0.171.0"
	const channel = "codex-hand-join"
	const thread = "codex-hand-joined-thread"
	w := newWorld(t)
	t.Cleanup(func() {
		if t.Failed() {
			w.logDispatcher()
		}
	})
	codexHome := filepath.Join(w.home, ".codex")
	fixtureBin := filepath.Join(w.home, "bin")
	if err := os.Mkdir(fixtureBin, 0700); err != nil {
		t.Fatal(err)
	}
	versionProbe := []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf '%s\\n' 'codex-cli " + version + "'\n")
	if err := os.WriteFile(filepath.Join(fixtureBin, "codex"), versionProbe, 0700); err != nil {
		t.Fatal(err)
	}
	installer := &participant{w: w, extra: []string{"PATH=" + fixtureBin + ":/usr/bin:/bin"}}
	installer.must("setup", "codex", "--apply")

	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", channel)
	owner.must("owner", "adapter", "set", "codex", "--enabled", "on")
	owner.must("owner", "publish", "hall", "on")
	evidence, err := json.Marshal(domain.AdapterEvidence{
		Vendor: "codex", VendorVersion: version, Mode: "daemon", ManifestRev: "codex-1",
		Platform: runtime.GOOS + "-" + runtime.GOARCH, RunAtMS: time.Now().UnixMilli(),
		Results: []domain.EvidenceResult{{Cap: "publish", Level: "tested", Check: "fixture managed queue for a hand join"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(w.home, "codex-wake-evidence.json")
	if err := os.WriteFile(evidencePath, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	owner.must("owner", "adapter", "evidence", evidencePath)
	sender := w.member()
	sender.must("join", channel, "--as", "sender")

	daemon := filepath.Join(codexHome, "packages", "app-server-daemon", "releases", version+"-"+arch+"-"+platform, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(daemon), 0700); err != nil {
		t.Fatal(err)
	}
	buildCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if raw, err := exec.CommandContext(buildCtx, "go", "test", "-c", "-o", daemon, "./testdata/linked_codex_daemon").CombinedOutput(); err != nil {
		t.Fatalf("build managed Codex fixture: %v\n%s", err, raw)
	}
	tmp, err := os.MkdirTemp("/tmp", "ch-codex-hand-join-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	socket := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	requests := filepath.Join(tmp, "requests.jsonl")
	cmd := exec.CommandContext(t.Context(), daemon, "-test.run=^TestManagedCodexDaemon$", "app-server", "--managed-daemon")
	cmd.Dir = w.home
	cmd.Env = append(w.baseEnv(), "TMPDIR="+tmp,
		"CROSSHALL_FIXTURE_CODEX_HOOK="+w.binaryPath(),
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

	// The session's start never ran; it joins by hand during a turn, and the
	// turn ends with no prompt after the join.
	receiver := &participant{w: w, extra: []string{"CODEX_THREAD_ID=" + thread}}
	receiver.must("join", channel, "--as", "receiver")
	db := w.db()
	if err := json.NewEncoder(input).Encode(map[string]any{
		"hook_event_name": "Stop", "session_id": thread, "cwd": w.home, "turn_id": "turn-1", "stop_hook_active": false,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn end registered the daemon route on the managed server", func() bool {
		var host int
		return db.QueryRow(`SELECT COALESCE(r.host_pid, 0) FROM publish_route r JOIN binding b ON b.id = r.binding_id
			WHERE b.alias = 'receiver' AND r.adapter = 'codex' AND r.state = 'active' AND r.mode = 'daemon' AND r.thread_id = ?`,
			thread).Scan(&host) == nil && host == cmd.Process.Pid
	})
	eventually(t, "the dispatcher proved the route on the managed server", func() bool {
		var activated bool
		return db.QueryRow(`SELECT p.activated_at_ms IS NOT NULL FROM publish_route_proof p JOIN publish_route r
			ON r.binding_id = p.binding_id AND r.generation = p.generation WHERE r.thread_id = ? AND p.process_pid = ?`,
			thread, cmd.Process.Pid).Scan(&activated) == nil && activated
	})
	var prompts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM observation WHERE kind = 'prompt_submitted'`).Scan(&prompts); err != nil || prompts != 0 {
		t.Fatalf("%d prompts recorded (%v); the route must come from the turn end alone", prompts, err)
	}

	messageID := num(t, sender.must("ask", "@receiver", "Check the hand-joined Codex queue."), "id")
	adapter, ok := publish.Lookup("codex")
	if !ok {
		t.Fatal("Codex publish adapter missing from the fixture binary")
	}
	want, ok := adapter.Notice([]int64{messageID})
	if !ok {
		t.Fatal("Codex did not format the notice")
	}
	eventually(t, "the ask was queued on the hand-joined thread", func() bool {
		raw, err := os.ReadFile(requests)
		if err != nil {
			return false
		}
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			var call struct {
				Method string
				Params struct {
					ThreadID string
					Input    []struct{ Type, Text string }
				}
			}
			if json.Unmarshal(line, &call) == nil && call.Method == "thread/queue/add" && call.Params.ThreadID == thread &&
				len(call.Params.Input) == 1 && call.Params.Input[0].Text == want {
				return true
			}
		}
		return false
	})
}
