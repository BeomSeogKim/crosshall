//go:build conformance

package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/appserver"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

type nativeCodexRelease struct {
	Version, Target, Entrypoint string
	dir                         string
}

func nativeCodexPackage(t *testing.T) nativeCodexRelease {
	t.Helper()
	path := os.Getenv("CROSSHALL_CONFORMANCE_CODEX_PACKAGE")
	if path == "" {
		t.Skip("set CROSSHALL_CONFORMANCE_CODEX_PACKAGE to a public managed Codex release directory")
	}
	if !filepath.IsAbs(path) {
		t.Fatal("CROSSHALL_CONFORMANCE_CODEX_PACKAGE must be an absolute path")
	}
	path, err := filepath.EvalSymlinks(path)
	codexMust(t, err)
	data, err := os.ReadFile(filepath.Join(path, "codex-package.json"))
	codexMust(t, err)
	release := nativeCodexRelease{dir: path}
	codexMust(t, json.Unmarshal(data, &release))
	if release.Entrypoint != "bin/codex" || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(release.Version) ||
		release.Target != "aarch64-apple-darwin" {
		t.Fatalf("expected a managed Codex release for macOS arm64, got %+v", release)
	}
	entryPath := filepath.Join(path, release.Entrypoint)
	physicalEntry, err := filepath.EvalSymlinks(entryPath)
	codexMust(t, err)
	entry, err := os.Lstat(entryPath)
	codexMust(t, err)
	if physicalEntry != entryPath || !entry.Mode().IsRegular() || entry.Mode().Perm()&0111 == 0 {
		t.Fatal("expected a regular Codex executable inside the managed release")
	}
	return release
}

type nativeCodexSession struct {
	client      *appserver.Client
	id, version string
}

func newNativeCodex(t *testing.T, release nativeCodexRelease, root, repo, exe string, env []string, modelURL string) *nativeCodexSession {
	t.Helper()
	codexMust(t, validateCodexEnv(env))
	physicalRoot, err := filepath.EvalSymlinks(root)
	codexMust(t, err)
	if root != physicalRoot || !slices.Contains(env, codexRootKey+"="+root) {
		t.Fatal("expected the physical private Codex fixture root")
	}
	endpoint, err := url.Parse(modelURL)
	codexMust(t, err)
	if endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		t.Fatal("expected the loopback fake model server origin")
	}
	home := filepath.Join(root, "codex")
	packages := filepath.Join(home, "packages", "app-server-daemon")
	releases := filepath.Join(packages, "releases")
	codexMust(t, os.MkdirAll(releases, 0700))
	installed := filepath.Join(releases, release.Version+"-"+release.Target)
	copyCtx, cancelCopy := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelCopy()
	copyPkg := exec.CommandContext(copyCtx, "/bin/cp", "-RP", release.dir, installed)
	copyPkg.Env = []string{}
	if out, err := copyPkg.CombinedOutput(); err != nil {
		t.Fatalf("copy managed Codex release: %v: %.1000s", err, out)
	}
	codexMust(t, os.Symlink(installed, filepath.Join(packages, "current")))
	bin := filepath.Join(installed, release.Entrypoint)
	config := fmt.Sprintf(`model = "fixture"
model_provider = "fixture"
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "file"
check_for_update_on_startup = false
approval_policy = "never"
sandbox_mode = %q
web_search = "disabled"
allow_login_shell = false
chatgpt_base_url = %q
openai_base_url = %q
[analytics]
enabled = false
[feedback]
enabled = false
[features]
hooks = true
apps = false
plugins = false
[shell_environment_policy]
inherit = "all"
[projects.%q]
trust_level = "trusted"
[model_providers.fixture]
name = "Local conformance model"
wire_api = "responses"
base_url = %q
requires_openai_auth = false
`, codexToolSandbox(), modelURL+"/offline", modelURL+"/offline", repo, modelURL+"/v1")
	codexPut(t, filepath.Join(home, "config.toml"), []byte(config))
	state := filepath.Join(home, "app-server-daemon")
	codexMust(t, os.Mkdir(state, 0700))
	codexPut(t, filepath.Join(state, "settings.json"), []byte(`{"remoteControlEnabled":false,"updater":{"autoUpdateEnabled":false}}`))
	setupCtx, cancelSetup := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSetup()
	setup := exec.CommandContext(setupCtx, exe, "setup", "codex", "--apply")
	setup.Dir, setup.Env = repo, slices.Clone(env)
	for i, entry := range setup.Env {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			setup.Env[i] = "PATH=" + filepath.Dir(bin) + string(os.PathListSeparator) + path
		}
	}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("install stock Codex hooks: %v: %.1500s", err, out)
	}

	alias := filepath.Join(home, "app-server-control", "app-server-control.sock")
	tmp, err := filepath.EvalSymlinks("/tmp")
	codexMust(t, err)
	// Codex uses a deterministic short path where the control alias exceeds the Unix socket limit.
	socketDir := filepath.Join(tmp, fmt.Sprintf("codex-daemon-%d", os.Geteuid()))
	socket := filepath.Join(socketDir, fmt.Sprintf("%x", sha256.Sum256([]byte(alias))))
	for _, path := range []string{alias, socket, socket + ".lock"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected an unused fixture socket path %s: %v", path, err)
		}
	}
	prefix := codexToolCommand(t, bin, env, endpoint.Port(), root)
	account, err := user.Current()
	codexMust(t, err)
	prefix[2] += fmt.Sprintf(`(deny file-read* file-write* (subpath %q))(allow network-bind network-inbound network-outbound (local unix-socket (path %q)) (remote unix-socket (path %q)) (local unix-socket (path %q)) (remote unix-socket (path %q)))(allow file-write* (literal %q) (literal %q) (literal %q))`, account.HomeDir, alias, alias, socket, socket, socketDir, socket, socket+".lock")
	vendorEnv := append(slices.Clone(env), "TERM=dumb", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CEILING_DIRECTORIES="+root)
	for _, name := range []string{"CONFIG", "CACHE", "DATA", "STATE"} {
		dir := filepath.Join(root, "xdg-"+strings.ToLower(name))
		codexMust(t, os.Mkdir(dir, 0700))
		vendorEnv = append(vendorEnv, "XDG_"+name+"_HOME="+dir)
	}
	command := func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, prefix[0], append(slices.Clone(prefix[1:]), args...)...)
		cmd.Dir, cmd.Env = repo, vendorEnv
		return cmd
	}
	versionCtx, cancelVersion := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancelVersion()
	if out, err := command(versionCtx, "--version").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "codex-cli "+release.Version {
		t.Fatalf("managed release version check: %v (context: %v): %.1000s", err, versionCtx.Err(), out)
	}
	logPath := filepath.Join(root, "codex-app-server.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	codexMust(t, err)
	// Keeping the managed process in the foreground gives the test exclusive ownership of its lifetime.
	cmd := command(context.Background(), "-c", "bypass_hook_trust=true", "app-server", "--listen", "unix://", "--managed-daemon")
	cmd.Stdout, cmd.Stderr = log, log
	err = cmd.Start()
	_ = log.Close()
	codexMust(t, err)
	client := waitNativeCodex(t, cmd, alias, socket, logPath)
	t.Cleanup(func() { _ = client.Close() })
	s := &nativeCodexSession{client: client, version: release.Version}
	request, err := client.ConformanceStart("thread/start", map[string]any{
		"cwd": repo, "approvalPolicy": "never", "sandbox": codexToolSandbox(),
		"config": map[string]bool{"bypass_hook_trust": true},
	})
	codexMust(t, err)
	for s.id == "" {
		event := s.read(t)
		if event.ID == request {
			s.id = event.Result.Thread.ID
			if s.id == "" {
				t.Fatal("native Codex did not return a thread ID")
			}
		}
	}
	return s
}

func waitNativeCodex(t *testing.T, cmd *exec.Cmd, alias, socket, logPath string) *appserver.Client {
	t.Helper()
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	owned := make(map[string]os.FileInfo)
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
		for path, original := range owned {
			current, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !os.SameFile(current, original) {
				t.Errorf("fixture socket artifact changed: %s", path)
				continue
			}
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	var dialError error
	for {
		if resolved, err := filepath.EvalSymlinks(alias); err == nil {
			if resolved != alias && resolved != socket {
				t.Fatalf("unexpected scratch control socket target %s", resolved)
			}
			start, err := proc.StartOf(cmd.Process.Pid)
			if err == nil {
				client, err := appserver.Dial(ctx, alias, appserver.Identity{PID: int64(cmd.Process.Pid), Start: start.Identity})
				if err == nil {
					if resolved == socket {
						for _, path := range []string{socket, socket + ".lock"} {
							if info, err := os.Lstat(path); err == nil {
								owned[path] = info
							}
						}
					}
					return client
				}
				dialError = err
			}
		}
		select {
		case <-done:
			log, _ := os.ReadFile(logPath)
			t.Fatalf("managed Codex exited before ready: %v: %.2000s", waitErr, log)
		case <-ctx.Done():
			log, _ := os.ReadFile(logPath)
			t.Fatalf("managed Codex control socket did not become ready: %v: %.2000s", dialError, log)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type nativeCodexEvent struct {
	sessionMessage
	Params struct {
		ThreadID, TurnID string
		Turn             struct{ ID, Status string }
		Item             struct{ Type, Text string }
	}
}

func (s *nativeCodexSession) read(t *testing.T) nativeCodexEvent {
	t.Helper()
	var event nativeCodexEvent
	codexMust(t, s.client.ConformanceRead(&event))
	if event.Error != nil {
		t.Fatalf("native Codex RPC failed: %d %s", event.Error.Code, event.Error.Message)
	}
	return event
}

func (s *nativeCodexSession) turn(t *testing.T, prompt string) string {
	t.Helper()
	codexMust(t, s.client.ConformanceDeadline(time.Now().Add(time.Minute)))
	request, err := s.client.ConformanceStart("turn/start", map[string]any{
		"threadId": s.id,
		"input":    []map[string]string{{"type": "text", "text": prompt}},
	})
	codexMust(t, err)
	return s.finishTurn(t, request)
}

func (s *nativeCodexSession) armAwait(t *testing.T) func() string {
	t.Helper()
	codexMust(t, s.client.ConformanceDeadline(time.Now().Add(10*time.Second)))
	var result struct {
		Thread struct {
			Status struct{ Type string }
		}
	}
	codexMust(t, s.client.Call("thread/read", map[string]string{"threadId": s.id}, &result))
	if status := result.Thread.Status.Type; status != "idle" {
		t.Fatalf("Codex thread %s before native notice: want idle, got %q", s.id, status)
	}
	return func() string {
		t.Helper()
		codexMust(t, s.client.ConformanceDeadline(time.Now().Add(time.Minute)))
		return s.finishTurn(t, 0)
	}
}

func (s *nativeCodexSession) finishTurn(t *testing.T, request int) string {
	t.Helper()
	var acknowledged, completed string
	answered := make(map[string]bool)
	for {
		event := s.read(t)
		if request != 0 && event.ID == request {
			acknowledged = event.Result.Turn.ID
			if acknowledged == "" {
				t.Fatal("native Codex did not acknowledge the submitted turn")
			}
		}
		if event.Params.ThreadID == s.id {
			switch event.Method {
			case "item/completed":
				if event.Params.Item.Type == "agentMessage" && event.Params.Item.Text == "OK" {
					answered[event.Params.TurnID] = true
				}
			case "turn/completed":
				turn := event.Params.Turn
				if turn.ID == "" || turn.Status != "completed" || !answered[turn.ID] {
					t.Fatalf("native Codex thread %s turn %q finished as %q, OK=%v", s.id, turn.ID, turn.Status, answered[turn.ID])
				}
				completed = turn.ID
			}
		}
		if completed != "" && (request == 0 || acknowledged != "") {
			if request != 0 && acknowledged != completed {
				t.Fatalf("native Codex finished turn %s after submitting %s", completed, acknowledged)
			}
			return completed
		}
	}
}
