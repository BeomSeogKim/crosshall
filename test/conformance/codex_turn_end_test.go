//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"database/sql"
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
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	turnEndVersionEnv = "CROSSHALL_CODEX_FIXTURE_VERSION"
	turnEndModeEnv    = "CROSSHALL_CODEX_FIXTURE_MODE"
	codexTurnEndLine  = "[crosshall] pending: 1"
)

// The exec transport check uses an exact, disposable launch profile. An
// ordinary Codex callback does not carry that profile, so even with the
// tested row present it must remain silent and leave the request unread.
func TestCodexExecTurnEndConformance(t *testing.T) {
	const version = "0.170.0"
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if platform != "darwin-arm64" {
		t.Skipf("no turn-end launch fixture for %s", platform)
	}
	bin, err := exec.LookPath("codex")
	if errors.Is(err, exec.ErrNotFound) {
		t.Skip("no codex binary on PATH")
	}
	codexMust(t, err)
	t.Setenv("CROSSHALL_CONFORMANCE_CROSSHALL", "")
	for _, name := range []string{"ordinary", "fixture-profile"} {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			codexMust(t, err)
			codexMust(t, os.Chmod(root, 0700))
			env := codexEnv(t, root)
			prefix := codexCommand(t, bin, env, "1")
			probe := exec.Command(prefix[0], append(slices.Clone(prefix[1:]), "--version")...)
			probe.Dir, probe.Env = root, env
			versionOutput, err := probe.Output()
			codexMust(t, err)
			if strings.TrimSpace(string(versionOutput)) != "codex-cli "+version {
				t.Skipf("no turn-end launch fixture for %s on %s", strings.TrimSpace(string(versionOutput)), platform)
			}

			manifestPath, err := filepath.Abs("../../internal/hook/manifests/codex.json")
			codexMust(t, err)
			raw, err := os.ReadFile(manifestPath)
			codexMust(t, err)
			var manifest codexInstall
			codexMust(t, json.Unmarshal(raw, &manifest))
			attested := name == "fixture-profile"
			var buildFlags []string
			if attested {
				buildFlags = append(buildFlags, "-overlay="+codexTurnEndOverlay(t, root, manifestPath, raw))
			}
			exe := codexBinary(t, root, buildFlags...)
			repo, state := filepath.Join(root, "repo"), filepath.Join(root, "state")
			codexMust(t, os.Mkdir(repo, 0700))
			git := exec.Command("git", "init", "-q", repo)
			git.Env = env
			codexMust(t, git.Run())
			startCodexDispatcher(t, exe, state, env)
			db := openCodexLedger(t, state)
			codexMust(t, codexCLI(exe, env, "owner", "channel", "create", "fixture"))
			codexMust(t, codexCLI(exe, env, "owner", "channel", "default", "fixture", "--repo", repo))
			codexMust(t, codexCLI(exe, env, "owner", "adapter", "set", "codex", "--enabled", "on"))
			peer := append(slices.Clone(env), "CROSSHALL_ROUTE=0123456789abcdef0123456789abcdef")
			codexMust(t, codexCLI(exe, peer, "join", "fixture", "--as", "gate-peer"))
			codexMust(t, codexCLI(exe, env, "owner", "budget", "grant", "fixture/@gate-peer", "--roots", "1"))
			evidence := codexEvidence{"codex", version, platform, "exec", manifest.Revision, time.Now().UnixMilli(), []codexResult{
				{"turn_end", "tested", "Fixture-only launch attestation for the isolated exec continuation check."},
			}}
			file, contents := codexEvidenceFile(t, root, evidence)
			codexMust(t, codexCLI(exe, env, "owner", "adapter", "evidence", file))
			codexVerifyImport(t, db, evidence, contents)
			stopInput, stopOutput := codexTurnEndHooks(t, root, exe, manifest)

			server, requests := startCodexTurnEndFixture(t, db, exe, peer)
			config := fmt.Sprintf("model_provider=\"fixture\"\nmodel=\"fixture\"\n[model_providers.fixture]\nname=\"Fixture\"\nwire_api=\"responses\"\nbase_url=%q\n", server.URL+"/v1")
			codexPut(t, filepath.Join(root, "codex", "config.toml"), []byte(config))
			endpoint, err := url.Parse(server.URL)
			codexMust(t, err)
			prefix = codexToolCommand(t, bin, env, endpoint.Port(), root)
			args := append(slices.Clone(prefix[1:]), "-a", "never", "exec", "--dangerously-bypass-hook-trust",
				"--skip-git-repo-check", "--sandbox", codexToolSandbox(), "Finish the fixture turn.")
			if attested {
				// The launcher just checked this same executable's exact build
				// and explicitly selected exec. Only the overlay consumes this
				// metadata, which no ordinary callback supplies.
				env = append(slices.Clone(env), turnEndVersionEnv+"="+version, turnEndModeEnv+"=exec")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, prefix[0], args...)
			cmd.Dir, cmd.Env = repo, env
			out, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(out)) != "OK" {
				var exitErr *exec.ExitError
				errors.As(err, &exitErr)
				if exitErr != nil {
					t.Logf("Codex stderr: %s", exitErr.Stderr)
				}
				t.Fatalf("codex exec: %v; got %q, want OK", err, out)
			}

			wantRequests, wantClaims := 1, 0
			wantVersion, wantMode := "unverified", "unverified"
			if attested {
				wantRequests, wantClaims = 2, 1
				wantVersion, wantMode = version, "exec"
			}
			got := requests()
			if len(got) != wantRequests {
				t.Fatalf("loopback model requests: got %d, want %d; pending line counts: %v", len(got), wantRequests, got)
			}
			if len(got[0]) != 0 {
				t.Fatalf("first model request contained the pending line before the Stop: %v", got[0])
			}
			if attested && !reflect.DeepEqual(got[1], map[string]map[string]int{"user": {"turn-end": 1}}) {
				t.Fatalf("continuation should contain one user-role pending line: %v", got[1])
			}
			verifyCodexTurnEndStops(t, stopInput, stopOutput, attested, wantRequests)
			var gotVersion, gotMode string
			codexMust(t, db.QueryRow(`SELECT json_extract(profile, '$.vendor_version'), json_extract(profile, '$.mode')
				FROM adapter_launch WHERE json_extract(profile, '$.adapter')='codex'`).Scan(&gotVersion, &gotMode))
			if gotVersion != wantVersion || gotMode != wantMode {
				t.Fatalf("callback profile = %s/%s, want %s/%s", gotVersion, gotMode, wantVersion, wantMode)
			}
			for _, check := range []struct {
				query string
				want  int
			}{
				{"SELECT count(*) FROM turn_end_notice WHERE adapter='codex'", wantClaims},
				{"SELECT count(*) FROM announcement WHERE via='turn_end' AND message_id=1", wantClaims},
				{"SELECT count(*) FROM recipient WHERE message_id=1 AND picked_up_at_ms IS NULL", 1},
			} {
				var count int
				codexMust(t, db.QueryRow(check.query).Scan(&count))
				if count != check.want {
					t.Errorf("%s: got %d, want %d", check.query, count, check.want)
				}
			}
		})
	}
}

func codexTurnEndOverlay(t *testing.T, root, manifest string, raw []byte) string {
	t.Helper()
	var doc map[string]json.RawMessage
	codexMust(t, json.Unmarshal(raw, &doc))
	doc["version_env"] = json.RawMessage(fmt.Sprintf("%q", turnEndVersionEnv))
	doc["version_pattern"] = json.RawMessage(`"^([0-9]+[.][0-9]+[.][0-9]+)$"`)
	doc["mode_env"] = json.RawMessage(fmt.Sprintf("%q", turnEndModeEnv))
	doc["modes"] = json.RawMessage(`{"exec":"exec"}`)
	contents, err := json.Marshal(doc)
	codexMust(t, err)
	replacement := filepath.Join(root, "codex-manifest.json")
	codexPut(t, replacement, contents)
	overlay := filepath.Join(root, "overlay.json")
	contents, err = json.Marshal(map[string]any{"Replace": map[string]string{manifest: replacement}})
	codexMust(t, err)
	codexPut(t, overlay, contents)
	return overlay
}

func codexTurnEndHooks(t *testing.T, root, exe string, manifest codexInstall) (string, string) {
	t.Helper()
	input, output := filepath.Join(root, "stop-input"), filepath.Join(root, "stop-output")
	hooks := map[string][]codexGroup{}
	for _, event := range manifest.Install.Events {
		command := codexQuote(exe) + " hook codex " + event
		if event == "Stop" {
			command = "/usr/bin/tee -a " + codexQuote(input) + " | " + command +
				" | /usr/bin/tee -a " + codexQuote(output) + "; printf '\\n' >> " + codexQuote(input)
		}
		hooks[event] = []codexGroup{{Matcher: manifest.Install.Matchers[event], Hooks: []codexHook{{"command", command, manifest.Install.Timeout}}}}
	}
	raw, err := json.Marshal(map[string]any{"hooks": hooks})
	codexMust(t, err)
	codexPut(t, filepath.Join(root, "codex", "hooks.json"), raw)
	return input, output
}

func startCodexTurnEndFixture(t *testing.T, db *sql.DB, exe string, peer []string) (*httptest.Server, func() []map[string]map[string]int) {
	t.Helper()
	var mu sync.Mutex
	var calls []map[string]map[string]int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		counts, err := codexCounts(raw, map[string]string{"turn-end": codexTurnEndLine})
		if err != nil {
			t.Errorf("decode Responses request: %v", err)
			http.Error(w, "invalid Responses request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls = append(calls, counts)
		n := len(calls)
		mu.Unlock()
		if n == 1 {
			// This request starts after UserPromptSubmit; deliver the ask
			// before the first model response, so only Stop can announce it.
			var alias string
			err = db.QueryRowContext(r.Context(), `SELECT b.alias FROM binding b JOIN principal p ON p.id=b.principal_id
				WHERE b.state='live' AND p.vendor='codex'`).Scan(&alias)
			if err == nil {
				err = codexCLI(exe, peer, "ask", "@"+alias, "Read this fixture request.", "--new")
			}
			if err != nil {
				t.Errorf("store mid-turn fixture request: %v", err)
				http.Error(w, "fixture request could not be stored", http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, codexResponseStream, n)
	}))
	t.Cleanup(server.Close)
	return server, func() []map[string]map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func verifyCodexTurnEndStops(t *testing.T, input, output string, attested bool, wantStops int) {
	t.Helper()
	raw, err := os.ReadFile(input)
	codexMust(t, err)
	type stop struct {
		Session string `json:"session_id"`
		Turn    string `json:"turn_id"`
		Active  *bool  `json:"stop_hook_active"`
	}
	var stops []stop
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var event stop
		err := dec.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		codexMust(t, err)
		stops = append(stops, event)
	}
	if len(stops) != wantStops || stops[0].Session == "" || stops[0].Turn == "" || stops[0].Active == nil || *stops[0].Active ||
		attested && (stops[1].Session != stops[0].Session || stops[1].Turn != stops[0].Turn || stops[1].Active == nil || !*stops[1].Active) {
		t.Fatalf("unexpected native Stop sequence: %s", raw)
	}
	raw, err = os.ReadFile(output)
	codexMust(t, err)
	if !attested {
		if len(raw) != 0 {
			t.Fatalf("ordinary Stop printed %q", raw)
		}
		return
	}
	var got map[string]string
	codexMust(t, json.Unmarshal(raw, &got))
	if want := map[string]string{"decision": "block", "reason": codexTurnEndLine}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Stop output = %v, want %v", got, want)
	}
}
