//go:build conformance

package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeCodeInteractiveIdleRepairsAbsorbedPrompt(t *testing.T) {
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	c.installInteractive()
	c.allowScratchPublish()

	started, release := filepath.Join(c.root, "tool-started"), filepath.Join(c.root, "tool-release")
	c.model.bash[2] = fmt.Sprintf("touch %q && for i in {1..300}; do if [ -e %q ]; then break; fi; sleep 0.1; done", started, release)
	term := c.interactive("--allowedTools", "Bash")
	term.await("the active publish route", func() bool {
		return c.storeCount(`SELECT COUNT(*) FROM publish_route WHERE adapter = 'claude' AND state = 'active'`) == 1
	})
	term.turn("warm-up synthetic prompt")

	term.idle()
	term.keys("run the synthetic tool\r")
	term.await("the tool to start", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	prompts := c.count(named("UserPromptSubmit"))
	const absorbed = "typed during the synthetic tool"
	term.keys(absorbed + "\r")
	term.await("the typed-ahead prompt callback", func() bool { return c.count(named("UserPromptSubmit")) == prompts+1 })
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	term.await("the Stop after the tool result", func() bool { return c.count(named("Stop")) == 2 })
	term.idle()

	const toolShape = "UserPromptSubmit A, Stop A, UserPromptSubmit B, UserPromptSubmit B, Stop B"
	if got := turnShape(c.events()); got != toolShape || c.model.count() != 3 ||
		!strings.Contains(c.model.request(3), "tool_result") || !strings.Contains(c.model.request(3), absorbed) {
		t.Fatalf("tool turn shape %q and %d model requests, want %q and the typed prompt beside the tool result", got, c.model.count(), toolShape)
	}
	openPlaceholders := func() int {
		return c.storeCount(`SELECT COUNT(*) FROM turn
			WHERE turn_ref LIKE 'typed-ahead:%' AND finishing_hook_at_ms IS NULL AND superseded = 0`)
	}
	term.await("the absorbed prompt's open placeholder", func() bool { return openPlaceholders() > 0 })

	first := c.writerSends("ask", "synthetic request before idle")
	outcome := func(id int64, observation string) int {
		return c.storeCount(`SELECT COUNT(*) FROM publish_attempt a JOIN publish_attempt_item i ON i.attempt_id = a.id
			WHERE i.message_id = ? AND a.observation = ?`, id, observation)
	}
	term.await("the first notice's unknown receipt", func() bool { return outcome(first, "unknown") == 1 })
	suspended := func() bool {
		return c.storeCount(`SELECT COUNT(*) FROM publish_suspension s
			JOIN publish_attempt_item i ON i.attempt_id = s.notice
			WHERE i.message_id = ? AND s.reason = 'unknown' AND (s.until_ms IS NULL OR s.until_ms > ?)`,
			first, time.Now().UnixMilli()) == 1
	}
	if !suspended() {
		t.Fatal("the unknown receipt did not suspend its launch")
	}

	term.awaitWithin(90*time.Second, "Claude Code's idle notification", func() bool {
		return c.count(func(e hookEvent) bool { return e.Name == "Notification" && e.Notice == "idle_prompt" }) > 0
	})
	term.await("the persisted idle fact", func() bool {
		return c.storeCount(`SELECT COUNT(*) FROM observation WHERE kind = 'idle_prompt'`) > 0
	})
	if got := openPlaceholders(); got != 0 || suspended() {
		t.Fatalf("after idle: %d open placeholders, unknown suspension active = %t", got, suspended())
	}

	later := c.writerSends("ask", "synthetic request after idle")
	term.await("the later notice's own-turn receipt", func() bool { return outcome(later, "own_turn") == 1 })
	term.exit()
}
