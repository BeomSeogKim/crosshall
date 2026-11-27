//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only the disposable vendor configuration registers PostToolUse and the
// once-only blocking Stop. The loopback request records what Codex delivered;
// the fixture has no access to a product claim or reminder implementation.
func TestCodexReminderTransport(t *testing.T) {
	bin, err := exec.LookPath("codex")
	if errors.Is(err, exec.ErrNotFound) {
		t.Skip("no codex binary on PATH")
	}
	codexMust(t, err)
	root, err := filepath.EvalSymlinks(t.TempDir())
	codexMust(t, err)
	codexMust(t, os.Chmod(root, 0700))
	env := codexEnv(t, root)
	// Recheck the measured tool matching and Stop envelope on the version
	// that supplied the observation. A different build needs its own probe.
	prefix := codexCommand(t, bin, env, "1")
	version := exec.Command(prefix[0], append(slices.Clone(prefix[1:]), "--version")...)
	version.Dir, version.Env = root, env
	output, err := version.Output()
	codexMust(t, err)
	measured, found := strings.CutPrefix(strings.TrimSpace(string(output)), "codex-cli ")
	if !found || !slices.Contains(codexReminderVersions, measured) || runtime.GOOS+"-"+runtime.GOARCH != "darwin-arm64" {
		t.Skipf("no reminder transport evidence for %s on %s/%s", strings.TrimSpace(string(output)), runtime.GOOS, runtime.GOARCH)
	}
	exe := codexBinary(t, root)
	repo, state := filepath.Join(root, "repo"), filepath.Join(root, "state")
	codexMust(t, os.Mkdir(repo, 0700))
	git := exec.Command("git", "init", "-q", repo)
	git.Env = env
	codexMust(t, git.Run())
	startCodexDispatcher(t, exe, state, env)
	codexMust(t, codexCLI(exe, env, "owner", "channel", "create", "fixture"))
	codexMust(t, codexCLI(exe, env, "owner", "channel", "default", "fixture", "--repo", repo))
	codexMust(t, codexCLI(exe, env, "owner", "adapter", "set", "codex", "--enabled", "on"))
	peer := append(slices.Clone(env), "CODEX_THREAD_ID=fixture-peer")
	codexMust(t, codexCLI(exe, peer, "join", "fixture", "--as", "gate-peer"))
	peerEvent := func(name, field, value string) error {
		input, err := json.Marshal(map[string]string{"hook_event_name": name, "session_id": "fixture-peer", "cwd": repo, field: value})
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "hook", "codex", name)
		cmd.Dir, cmd.Env, cmd.Stdin = repo, peer, bytes.NewReader(input)
		return cmd.Run()
	}
	codexMust(t, peerEvent("SessionStart", "source", "startup"))
	db := openCodexLedger(t, state)

	var manifest codexInstall
	raw, err := os.ReadFile("../../internal/hook/manifests/codex.json")
	codexMust(t, err)
	codexMust(t, json.Unmarshal(raw, &manifest))
	const reason = "[crosshall] you picked up 1 and have not replied. Reply, decline, or ask for more time."
	stop, err := json.Marshal(map[string]string{"decision": "block", "reason": reason})
	codexMust(t, err)
	codexPut(t, filepath.Join(root, "claim.json"), stop)
	hooks := map[string][]map[string]any{}
	add := func(event, matcher, command string) map[string]any {
		handler := map[string]any{"type": "command", "command": command, "timeout": 1}
		group := map[string]any{"hooks": []any{handler}}
		if matcher != "" {
			group["matcher"] = matcher
		}
		hooks[event] = append(hooks[event], group)
		return handler
	}
	for _, event := range manifest.Install.Events {
		add(event, manifest.Install.Matchers[event], codexQuote(exe)+" hook codex "+event)
	}
	record := func(file string) string {
		path := codexQuote(filepath.Join(root, file))
		return "/bin/cat >> " + path + " && printf '\\n' >> " + path
	}
	add("PostToolUse", "^Bash$", record("post-events"))
	add("PostToolUse", "^Bash\\(.*crosshall.*read.*\\)$", record("command-matches"))
	add("PostToolUse", "^Bash$", record("condition-events"))["if"] = "Bash(printf *)"
	add("Stop", "", record("stops")+" && if /bin/mkdir "+codexQuote(filepath.Join(root, "claimed"))+" 2>/dev/null; then /bin/cat "+codexQuote(filepath.Join(root, "claim.json"))+"; fi")
	raw, err = json.Marshal(map[string]any{"hooks": hooks})
	codexMust(t, err)
	codexPut(t, filepath.Join(root, "codex", "hooks.json"), raw)

	const body = "synthetic addressed read for U8"
	commands := []string{codexQuote(exe) + " who", codexQuote(exe) + " read 1", "printf '%s%s' fixture- shell-ok"}
	var lock sync.Mutex
	var requests [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "invalid input", http.StatusBadRequest)
			return
		}
		lock.Lock()
		requests = append(requests, raw)
		n := len(requests)
		lock.Unlock()
		if n == 1 {
			var alias string
			err := db.QueryRow("SELECT alias FROM binding JOIN principal ON principal.id=binding.principal_id WHERE vendor='codex' AND state='live' AND alias<>'gate-peer'").Scan(&alias)
			if err == nil {
				err = codexCLI(exe, peer, "ask", "@"+alias, body, "--new")
			}
			if err != nil {
				t.Errorf("address fixture ask: %v", err)
				http.Error(w, "fixture setup failed", http.StatusBadRequest)
				return
			}
		} else if n == 2 {
			// A peer prompt after 'who' makes the next ordinary read show
			// the coalesced presence change once.
			if err := peerEvent("UserPromptSubmit", "turn_id", "peer-turn"); err != nil {
				t.Errorf("peer prompt: %v", err)
				http.Error(w, "fixture setup failed", http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= len(commands) {
			codexFixtureShell(w, fmt.Sprintf("call_fixture_%d", n), commands[n-1])
		} else {
			fmt.Fprintf(w, codexResponseStream, n)
		}
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	codexMust(t, err)
	config := fmt.Sprintf("model_provider=\"fixture\"\nmodel=\"fixture\"\n[shell_environment_policy]\ninherit=\"all\"\n[model_providers.fixture]\nname=\"fixture\"\nwire_api=\"responses\"\nbase_url=%q\n", server.URL+"/v1")
	codexPut(t, filepath.Join(root, "codex", "config.toml"), []byte(config))
	prefix = codexToolCommand(t, bin, env, endpoint.Port(), root)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prefix[0], append(slices.Clone(prefix[1:]), "-a", "never", "exec", "--dangerously-bypass-hook-trust", "--skip-git-repo-check", "--sandbox", codexToolSandbox(), "Complete this fixture turn.")...)
	cmd.Dir, cmd.Env = repo, env
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("codex exec: %v; expected OK", err)
	}

	// After cmd exits, no further requests can enter the fixture.
	lock.Lock()
	defer lock.Unlock()
	if len(requests) != len(commands)+2 {
		t.Fatalf("got %d model requests, want three tools, completion and one continuation", len(requests))
	}
	for i, request := range requests {
		counts, err := codexCounts(request, map[string]string{"reminder": reason})
		codexMust(t, err)
		last := i == len(requests)-1
		if last && (len(counts) != 1 || counts["user"]["reminder"] != 1) || !last && len(counts) != 0 {
			t.Fatalf("request %d unexpected reminder roles: %v", i+1, counts)
		}
	}
	logRequestTokenSamples(t, "codex", requests[2], "presence")
	logRequestTokenSamples(t, "codex", requests[len(requests)-1], "reminder")
	type callback struct {
		ToolName string `json:"tool_name"`
		Call     string `json:"tool_use_id"`
		Turn     string `json:"turn_id"`
		Result   string `json:"tool_response"`
		Active   bool   `json:"stop_hook_active"`
	}
	readEvents := func(file string) []callback {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, file))
		codexMust(t, err)
		dec := json.NewDecoder(bytes.NewReader(b))
		var events []callback
		for {
			var event callback
			err := dec.Decode(&event)
			if err == io.EOF {
				return events
			}
			codexMust(t, err)
			events = append(events, event)
		}
	}
	events := readEvents("post-events")
	if len(events) != len(commands) {
		t.Fatalf("got %d post-tool starts for %d shell calls", len(events), len(commands))
	}
	for i, expected := range []string{"@gate-peer", body, "fixture-shell-ok"} {
		event := events[i]
		if event.ToolName != "Bash" || event.Call != fmt.Sprintf("call_fixture_%d", i+1) || event.Turn == "" || event.Turn != events[0].Turn || !strings.Contains(event.Result, expected) {
			t.Fatalf("tool %d missing its native ID, shared turn, or successful output %q: %q", i+1, expected, event.Result)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "command-matches")); !os.IsNotExist(err) {
		t.Fatalf("command-shaped matcher unexpectedly ran: %v", err)
	}
	if count := len(readEvents("condition-events")); count != len(commands) {
		t.Fatalf("the host ran the candidate 'if' hook %d times; re-evaluate command-level filtering", count)
	}
	stops := readEvents("stops")
	if len(stops) != 2 || stops[0].Active || !stops[1].Active || stops[0].Turn != events[0].Turn || stops[1].Turn != stops[0].Turn {
		t.Fatalf("unexpected continuation boundaries: %+v", stops)
	}
	var pickups int
	codexMust(t, db.QueryRow("SELECT count(*) FROM recipient WHERE message_id=1 AND picked_up_at_ms IS NOT NULL").Scan(&pickups))
	if pickups != 1 {
		t.Fatalf("real read made %d pickups, want one", pickups)
	}
	t.Log("three shell calls, three Bash callbacks, one actual read, two Stops in the same turn, five model requests including one controlled user-role continuation")
}
