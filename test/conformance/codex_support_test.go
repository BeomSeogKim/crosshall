//go:build conformance

package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

var codexPaths = map[string]string{
	"HOME": "home", "CODEX_HOME": "codex", "CLAUDE_CONFIG_DIR": "claude",
	"CROSSHALL_STATE_DIR": "state", "TMPDIR": "tmp",
}

const codexRootKey = "CROSSHALL_CONFORMANCE_ROOT"

func codexPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("expected a private directory owned by this user: %s", path)
	}
	return nil
}

func codexCheckRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("expected an absolute conformance root")
	}
	if err := codexPrivateDir(root); err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	bases := []string{"/tmp", "/var/tmp"}
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR")
		cmd.Env = []string{}
		if output, err := cmd.Output(); err == nil {
			bases = append(bases, strings.TrimSpace(string(output)))
		}
	}
	for _, base := range bases {
		if dir, err := filepath.EvalSymlinks(base); err == nil && strings.HasPrefix(physical, dir+"/") {
			return nil
		}
	}
	return errors.New("conformance root must be below a system temporary directory")
}

func codexEnv(t *testing.T, root string) []string {
	t.Helper()
	if err := codexCheckRoot(root); err != nil {
		t.Fatal(err)
	}
	env := []string{codexRootKey + "=" + root, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	for key, child := range codexPaths {
		path := filepath.Join(root, child)
		if key == "CROSSHALL_STATE_DIR" {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected fresh dispatcher state at %s", path)
			}
		} else if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		env = append(env, key+"="+path)
	}
	return env
}

func validateCodexEnv(env []string) error {
	values := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if _, duplicate := values[key]; !ok || duplicate {
			return fmt.Errorf("invalid or duplicate fixture variable %q", key)
		}
		values[key] = value
	}
	root := values[codexRootKey]
	if err := codexCheckRoot(root); err != nil {
		return err
	}
	for key, child := range codexPaths {
		path := filepath.Join(root, child)
		if values[key] != path {
			return fmt.Errorf("missing or unexpected fixture path for %s", key)
		}
		if err := codexPrivateDir(path); err != nil && !(key == "CROSSHALL_STATE_DIR" && errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	return nil
}

func codexCommand(t *testing.T, bin string, env []string, port string) []string {
	t.Helper()
	if err := validateCodexEnv(env); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 || !filepath.IsAbs(bin) {
		t.Fatal("expected a numeric loopback port and an absolute executable path")
	}
	executable, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		t.Fatal("cannot determine the account's real home")
	}
	configs := []string{filepath.Join(account.HomeDir, ".codex"), filepath.Join(account.HomeDir, ".claude")}
	releases := filepath.Join(configs[0], "packages", "standalone", "releases")
	binDir := filepath.Dir(executable)
	packaged := filepath.Dir(filepath.Dir(binDir)) == releases && filepath.Base(binDir) == "bin" && filepath.Base(executable) == "codex"

	for _, config := range configs {
		if (executable == config || strings.HasPrefix(executable, config+"/")) && !packaged {
			t.Fatalf("select a public vendor executable outside %s", config)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		profile := fmt.Sprintf(`(version 1)(allow default)(deny network*)(allow network-outbound (remote ip "localhost:%d"))(deny file-read* file-write* (subpath %q) (subpath %q))`, number, configs[0], configs[1])
		if packaged {
			profile += fmt.Sprintf("(allow file-read* (literal %q))", executable)
		}
		return []string{"/usr/bin/sandbox-exec", "-p", profile, executable}
	case "linux":
		interfaces, err := net.Interfaces()
		if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" || interfaces[0].Flags&net.FlagLoopback == 0 {
			t.Fatal("use an isolated Linux network namespace with only lo")
		}
		for _, config := range configs {
			if _, err := os.Lstat(config); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("use a disposable Linux account or container with no real vendor homes mounted")
			}
		}
		return []string{executable}
	default:
		t.Fatal("Codex conformance requires macOS or a loopback-only Linux namespace")
		return nil
	}
}

func TestCodexIsolation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	env := codexEnv(t, root)
	if err := validateCodexEnv(env); err != nil {
		t.Fatal(err)
	}
	for i, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key == "PATH" {
			continue
		}
		reduced := append([]string(nil), env[:i]...)
		reduced = append(reduced, env[i+1:]...)
		if validateCodexEnv(reduced) == nil {
			t.Errorf("accepted missing %s", key)
		}
	}
	if err := os.Symlink(filepath.Join(root, "home"), filepath.Join(root, "state")); err != nil {
		t.Fatal(err)
	}
	if validateCodexEnv(env) == nil {
		t.Error("accepted symlink in place of the fixture state directory")
	}
}

type codexFixture struct {
	URL      string
	mu       sync.Mutex
	requests []map[string]map[string]int
}

func (f *codexFixture) Requests() []map[string]map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make([]map[string]map[string]int, len(f.requests))
	for i, counts := range f.requests {
		copied[i] = make(map[string]map[string]int)
		for role, labels := range counts {
			copied[i][role] = maps.Clone(labels)
		}
	}
	return copied
}

func startCodexFixture(t *testing.T, markers map[string]string) *codexFixture {
	t.Helper()
	markers = maps.Clone(markers)
	for label, marker := range markers {
		if marker == "" {
			t.Fatalf("empty conformance marker %q", label)
		}
	}
	f := &codexFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		counts, err := codexCounts(body, markers)
		if err != nil {
			http.Error(w, "invalid Responses input", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, counts)
		number := len(f.requests)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, codexResponseStream, number)
	}))
	t.Cleanup(server.Close)
	f.URL = server.URL + "/v1"
	return f
}

func codexCounts(body []byte, markers map[string]string) (map[string]map[string]int, error) {
	var request struct {
		Input []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if request.Input == nil {
		return nil, errors.New("expected message list")
	}
	counts := make(map[string]map[string]int)
	for _, message := range request.Input {
		switch message.Role {
		case "user", "developer", "system", "assistant":
		default:
			continue
		}
		if len(message.Content) == 0 {
			continue
		}
		var parts []struct{ Text string }
		if message.Content[0] == '"' {
			var text string
			if err := json.Unmarshal(message.Content, &text); err != nil {
				return nil, err
			}
			parts = append(parts, struct{ Text string }{text})
		} else if err := json.Unmarshal(message.Content, &parts); err != nil {
			return nil, err
		}
		for _, part := range parts {
			for label, marker := range markers {
				if n := strings.Count(part.Text, marker); n > 0 {
					if counts[message.Role] == nil {
						counts[message.Role] = make(map[string]int)
					}
					counts[message.Role][label] += n
				}
			}
		}
	}
	return counts, nil
}

// The fixture supplies one final message per request and never asks for a tool.
const codexResponseStream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_%[1]d","model":"fixture","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_%[1]d","role":"assistant","status":"in_progress","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_%[1]d","output_index":0,"content_index":0,"delta":"OK"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_%[1]d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_%[1]d","model":"fixture","status":"completed","output":[{"type":"message","id":"msg_%[1]d","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}]}}

`

// Codex evidence is written only outside the whole login home, under the
// same rule as the Claude Code run.
func TestCodexEvidenceStaysOutsideTheHome(t *testing.T) {
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		t.Skip("no login home to check against")
	}
	if _, err := codexEvidenceDir(account.HomeDir); err == nil {
		t.Error("the login home was accepted for Codex evidence")
	}
	plain, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codexEvidenceDir(plain); err != nil {
		t.Errorf("a plain temporary directory was refused: %v", err)
	}
}
