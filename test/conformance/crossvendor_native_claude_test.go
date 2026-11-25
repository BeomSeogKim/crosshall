//go:build conformance

package conformance

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/testreap"
)

type nativeClaudeSession struct {
	term        *terminal
	id, version string
}

func newNativeClaude(t *testing.T, root, repo, exe string, env []string, entry string, server *httptest.Server) *nativeClaudeSession {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	codexMust(t, err)
	if endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" {
		t.Fatal("Claude fixture requires a loopback HTTP model")
	}
	bin, err := exec.LookPath("claude")
	codexMust(t, err)
	prefix := codexToolCommand(t, bin, env, endpoint.Port(), root)
	tmp := filepath.Join(root, "tmp")
	if runtime.GOOS == "darwin" {
		// The dispatcher connects to this listener. Claude only needs permission to bind and receive there.
		path := strings.ReplaceAll(regexp.QuoteMeta(tmp), `"`, `\"`)
		prefix[2] += fmt.Sprintf(`(allow network-bind network-inbound (local unix-socket (path-regex #"^%s/cc-socks(-[0-9]+)?/[0-9]+[.]sock$")))`, path)
	}
	c := &conformance{
		t: t, root: filepath.Join(root, "claude-events"), home: filepath.Join(root, "claude-home"),
		state: filepath.Join(root, "state"), config: filepath.Join(root, "claude"),
		repo: repo, crosshall: exe, model: &fixture{server: server},
	}
	for _, dir := range []string{c.root, c.home} {
		codexMust(t, os.Mkdir(dir, 0700))
	}
	claudeEnv := slices.DeleteFunc(c.claudeEnv(), func(kv string) bool {
		return strings.HasPrefix(kv, "TMPDIR=") || strings.HasPrefix(kv, testreap.Key+"=")
	})
	claudeEnv = append(claudeEnv, entry, "TMPDIR="+tmp, "CLAUDE_CODE_TMPDIR="+tmp, "XDG_RUNTIME_DIR="+tmp)
	version := exec.CommandContext(t.Context(), prefix[0], append(slices.Clone(prefix[1:]), "--version")...)
	version.Dir, version.Env = repo, claudeEnv
	out, err := version.CombinedOutput()
	if err != nil {
		t.Fatalf("claude --version: %v\n%s", err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatal("claude --version returned no version")
	}
	c.installInteractive()
	cmd := exec.CommandContext(t.Context(), prefix[0], append(slices.Clone(prefix[1:]),
		"--model", "haiku", "--permission-mode", "default", "--allowedTools", "Bash")...)
	term := c.startInteractive(cmd, claudeEnv)
	for _, event := range c.events() {
		if event.Name == "SessionStart" && event.Session != "" {
			return &nativeClaudeSession{term: term, id: event.Session, version: fields[0]}
		}
	}
	t.Fatal("Claude SessionStart did not report a session ID")
	return nil
}

func (s *nativeClaudeSession) turn(t *testing.T, prompt string) string {
	t.Helper()
	finish := s.armAwait(t)
	s.term.typeKeys(t, prompt+"\r")
	return finish()
}

func (s *nativeClaudeSession) armAwait(t *testing.T) func() string {
	t.Helper()
	s.term.awaitWithinFor(t, interactiveWait, "an idle Claude prompt", s.term.isIdle)
	before := len(s.term.c.events())
	return func() string {
		t.Helper()
		var turn string
		s.term.awaitWithinFor(t, interactiveWait, "a new completed Claude turn", func() bool {
			events := inSession(s.term.c.events()[before:], s.id)
			prompts := make(map[string]bool)
			for _, e := range events {
				if e.Name == "UserPromptSubmit" && e.Prompt != "" {
					prompts[e.Prompt] = true
				}
			}
			for _, e := range events {
				if e.Name == "Stop" && !e.StopActive && prompts[e.Prompt] {
					turn = e.Prompt
					return true
				}
			}
			return false
		})
		return turn
	}
}
