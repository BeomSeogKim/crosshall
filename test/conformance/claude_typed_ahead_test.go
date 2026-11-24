//go:build conformance

package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func typedAheadWorld(t *testing.T) *conformance {
	t.Helper()
	c := newConformance(t)
	c.requireClaude()
	startTypedAheadDispatcher(c)
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	c.installInteractive()
	return c
}

// Keep cold dispatcher startup outside the public owner-start deadline in
// these tests, whose timing assertions begin after the first vendor prompt.
func startTypedAheadDispatcher(c *conformance) {
	c.t.Helper()
	cmd := exec.CommandContext(c.t.Context(), c.crosshall, "__dispatch")
	cmd.Dir, cmd.Env = c.state, c.base()
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Second
	logPath := filepath.Join(c.state, "dispatcher.log")
	log, err := os.Create(logPath)
	if err != nil {
		c.t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start fixture dispatcher: %v", err)
	}
	done := make(chan struct{})
	var childErr error
	go func() {
		childErr = cmd.Wait()
		close(done)
	}()
	c.t.Cleanup(func() { <-done })

	marker := filepath.Join(ident.Rendezvous(c.state, os.Geteuid()), "ready")
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		} else if !os.IsNotExist(err) {
			c.t.Fatalf("check fixture dispatcher readiness: %v", err)
		}
		select {
		case <-done:
			out, _ := os.ReadFile(logPath)
			c.t.Fatalf("fixture dispatcher exited before publishing its ready marker: %v\n%s", childErr, out)
		case <-timeout.C:
			out, _ := os.ReadFile(logPath)
			c.t.Fatalf("fixture dispatcher did not publish its ready marker within 10s\n%s", out)
		case <-poll.C:
		}
	}
}

type promptBlock struct {
	Type    string
	Text    string
	ToolID  string `json:"tool_use_id"`
	IsError bool   `json:"is_error"`
}

// The request contains the entire history. Only its last user message tells
// us how Claude Code delivered the prompts that arrived during this turn.
func (c *conformance) secondRequestBlocks() []promptBlock {
	c.t.Helper()
	var req struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(c.model.request(2)), &req); err != nil {
		c.t.Fatal(err)
	}
	for len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "system" {
		req.Messages = req.Messages[:len(req.Messages)-1]
	}
	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != "user" {
		c.t.Fatal("expected a new user message in the second model request")
	}
	content := req.Messages[len(req.Messages)-1].Content
	var text string
	if json.Unmarshal(content, &text) == nil {
		return []promptBlock{{Type: "text", Text: text}}
	}
	var blocks []promptBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		c.t.Fatal(err)
	}
	return blocks
}

func (c *conformance) checkTypedAheadShape(want string) {
	c.t.Helper()
	all := c.events()
	if len(all) == 0 || all[0].Session == "" || len(inSession(all, all[0].Session)) != len(all) {
		c.t.Fatal("expected callbacks from one session")
	}
	if got, n := turnShape(all), c.model.count(); got != want || n != 2 {
		c.t.Fatalf("callbacks %q with %d model requests, want %q with 2", got, n, want)
	}
}

// While the model has the first turn, one or more queued prompts become
// a single subsequent turn. Its Stop has an ID no submit callback carried.
func TestClaudeCodeInteractiveTypedAheadPrompt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prompts []string
		shape   string
	}{
		{"one", []string{"second synthetic prompt"}, typedAheadShape},
		{"two", []string{"second synthetic prompt", "third synthetic prompt"}, "UserPromptSubmit A, UserPromptSubmit A, UserPromptSubmit A, Stop A, Stop B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := typedAheadWorld(t)
			release := make(chan struct{})
			defer close(release)
			c.model.during[1] = func() { <-release }
			term := c.interactive()
			term.keys("first synthetic prompt\r")
			term.await("the first model request", func() bool { return c.model.count() == 1 })
			for i, prompt := range tc.prompts {
				term.keys(prompt + "\r")
				term.await("the typed-ahead prompt's callback", func() bool { return c.count(named("UserPromptSubmit")) == i+2 })
			}
			release <- struct{}{}
			term.await("both Stops", func() bool { return c.count(named("Stop")) >= 2 })
			term.exit()
			c.checkTypedAheadShape(tc.shape)
			blocks := c.secondRequestBlocks()
			if len(blocks) != len(tc.prompts) {
				t.Fatalf("queued prompts produced %d blocks, want %d", len(blocks), len(tc.prompts))
			}
			for i, prompt := range tc.prompts {
				if blocks[i].Type != "text" || strings.TrimSpace(blocks[i].Text) != prompt {
					t.Fatalf("queued prompt %d: got %+v, want a text block containing %q", i+1, blocks[i], prompt)
				}
			}
		})
	}
}

// On Claude Code 2.1.330 a prompt typed during Bash travels beside the tool
// result. The single Stop is followed about a minute later by idle_prompt.
func TestClaudeCodeInteractiveTypedAheadDuringTool(t *testing.T) {
	c := typedAheadWorld(t)
	// Release the tool only after the submit hook ran. Its shell also has a
	// deadline so a failed test cannot leave it waiting indefinitely.
	const command = "touch tool-started && for i in $(seq 1 300); do if test -f tool-release; then exit 0; fi; sleep 0.1; done; exit 1"
	c.model.bash[1] = command
	term := c.interactive("--allowedTools", "Bash")
	term.keys("first synthetic prompt\r")
	term.await("Bash starting", func() bool {
		_, err := os.Stat(filepath.Join(c.repo, "tool-started"))
		return err == nil
	})
	const prompt = "second synthetic prompt"
	term.keys(prompt + "\r")
	term.await("the typed-ahead prompt's callback", func() bool { return c.count(named("UserPromptSubmit")) == 2 })
	if err := os.WriteFile(filepath.Join(c.repo, "tool-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	term.await("Stop after the tool result", func() bool { return c.count(named("Stop")) > 0 })
	blocks := c.secondRequestBlocks()
	if len(blocks) != 2 || blocks[0].Type != "tool_result" || blocks[0].ToolID != "toolu_fixture01" || blocks[0].IsError ||
		blocks[1].Type != "text" || !strings.Contains(blocks[1].Text, prompt) {
		t.Fatalf("expected the successful Bash result followed by the typed prompt, got %+v", blocks)
	}
	// Each probe record was last written when its callback was captured.
	// The test goroutine may first observe either record much later.
	session := c.events()[0].Session
	const latestIdle = 90 * time.Second
	var elapsed time.Duration
	term.awaitWithin(latestIdle, "idle_prompt after Stop", func() bool {
		var stop hookEvent
		for _, event := range inSession(c.events(), session) {
			if event.Name == "Stop" {
				stop = event
			}
			if stop.record != "" && event.Name == "Notification" && event.Notice == "idle_prompt" {
				elapsed = event.captureTime(t).Sub(stop.captureTime(t))
				return true
			}
		}
		return false
	})
	if elapsed < 55*time.Second || elapsed > latestIdle {
		t.Fatalf("idle_prompt arrived %s after Stop, want 55s to %s", elapsed, latestIdle)
	}
	term.exit()
	c.checkTypedAheadShape("UserPromptSubmit A, UserPromptSubmit A, Stop A")
}

func (event hookEvent) captureTime(t *testing.T) time.Time {
	t.Helper()
	info, err := os.Stat(event.record)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}
