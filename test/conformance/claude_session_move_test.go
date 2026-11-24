//go:build conformance

package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClaudeCodeInteractiveSessionMove(t *testing.T) {
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "create", "inventory")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	inventory := func(args ...string) string {
		t.Helper()
		out, err := c.crosshallRun([]string{"CROSSHALL_ROUTE=" + strings.Repeat("b", 32)}, args...)
		if err != nil {
			t.Fatalf("inventory %v: %v\n%s", args, err, out)
		}
		return out
	}
	inventory("join", "inventory", "--as", "peer")
	c.installInteractive()
	version := c.version()
	c.importTurnEnd(version, "interactive", "tested", "scratch hall only: exercises the destination Stop hook")

	verifyPublish := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
	if verifyPublish {
		evidence, err := json.Marshal(map[string]any{
			"vendor": "claude", "vendor_version": version, "platform": "darwin-arm64",
			"mode": "interactive", "manifest_rev": claudeManifestRevision(), "run_at_ms": time.Now().UnixMilli(),
			"results": []evidenceResult{{"publish", "tested", "scratch hall only: exercises the destination native route"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(c.root, "publish-session-move.json")
		if err := os.WriteFile(file, evidence, 0600); err != nil {
			t.Fatal(err)
		}
		c.owner("owner", "adapter", "evidence", file)
	}

	term := c.interactive("--allowedTools", "Bash")
	first := c.events()[0]
	if first.Name != "SessionStart" || first.Source != "startup" || first.Session == "" || first.Process == "" {
		t.Fatalf("initial native callback: %+v", first)
	}

	type commandResult struct {
		ID      int64
		Channel string
		Alias   string
		Move    *struct{ Channel string }
	}
	decode := func(out string) commandResult {
		t.Helper()
		var got commandResult
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode command result: %v\n%s", err, out)
		}
		return got
	}
	messageID := func(out string) int64 {
		t.Helper()
		id := decode(out).ID
		if id == 0 {
			t.Fatalf("command returned no message ID:\n%s", out)
		}
		return id
	}
	shellArg := func(arg string) string {
		return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	nativeCommand := func(prompt string, args ...string) commandResult {
		t.Helper()
		file := filepath.Join(c.root, fmt.Sprintf("native-command-%d.json", c.model.count()))
		command := shellArg(c.crosshall) + " --json"
		for _, arg := range args {
			command += " " + shellArg(arg)
		}
		c.model.mu.Lock()
		c.model.bash[len(c.model.requests)+1] = command + " > " + shellArg(file)
		c.model.mu.Unlock()
		term.turn(prompt)
		out, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("native Bash command %v: %v", args, err)
		}
		return decode(string(out))
	}

	var principal, source, launch int
	term.await("the native checkout membership", func() bool {
		principal = c.storeCount("SELECT COALESCE(MAX(principal_id), 0) FROM principal_key WHERE vendor = 'claude' AND key = ? AND state = 'live'", first.Session)
		source = c.storeCount("SELECT COALESCE(MAX(b.id), 0) FROM binding b JOIN channel ch ON ch.id = b.channel_id "+
			"WHERE b.principal_id = ? AND ch.name = 'checkout' AND b.state = 'live'", principal)
		launch = c.storeCount("SELECT COALESCE(MAX(rowid), 0) FROM adapter_launch WHERE principal_id = ? AND ended_hook_at_ms IS NULL", principal)
		return principal != 0 && source != 0 && launch != 0
	})
	outbound := nativeCommand("Run the checkout tool", "ask", "@writer", "Please reply after the move")
	if outbound.ID == 0 {
		t.Fatal("the original native member's ask returned no message ID")
	}

	waitForProof := func(binding int, label string) {
		t.Helper()
		term.await(label, func() bool {
			return c.storeCount("SELECT COUNT(*) FROM publish_route r JOIN publish_route_proof p "+
				"ON p.binding_id = r.binding_id AND p.generation = r.generation "+
				"JOIN adapter_launch l ON l.principal_id = r.principal_id AND l.launch_ref = r.launch_ref "+
				"WHERE r.binding_id = ? AND l.rowid = ? AND r.state = 'active' AND p.activated_at_ms >= r.registered_at_ms",
				binding, launch) == 1
		})
	}
	if verifyPublish {
		waitForProof(source, "the original native publish proof")
	}
	c.owner("owner", "publish", "hall", "off")
	moved := nativeCommand("Run the channel move tool", "join", "inventory", "--move", "--as", "moved")
	if moved.Channel != "inventory" || moved.Alias != "moved" || moved.Move == nil || moved.Move.Channel != "checkout" {
		t.Fatalf("native move result: %+v", moved)
	}
	destination := c.storeCount("SELECT COALESCE(MAX(b.id), 0) FROM member_selection s "+
		"JOIN binding b ON b.id = s.binding_id JOIN channel ch ON ch.id = b.channel_id "+
		"WHERE s.principal_id = ? AND s.generation > 1 AND ch.name = 'inventory' AND b.state = 'live'", principal)
	if destination == 0 || destination == source {
		t.Fatalf("invalid inventory binding %d for native member %d (was %d)", destination, principal, source)
	}
	if n := c.storeCount("SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed'", source); n != 1 {
		t.Fatal("the source binding did not depart")
	}

	late := messageID(c.writer("--json", "reply", strconv.FormatInt(outbound.ID, 10), "Late reply in checkout"))
	note := messageID(inventory("--json", "note", "@moved", "Inventory prompt marker"))
	promptRequest := c.model.count() + 1
	stopAsk := make(chan int64, 1)
	c.model.mu.Lock()
	c.model.during[promptRequest] = func() {
		stopAsk <- messageID(inventory("--json", "ask", "@moved", "Inventory Stop marker"))
	}
	c.model.mu.Unlock()
	term.submit("Read the inventory prompt context", func(event hookEvent) bool {
		return event.Name == "Stop" && event.StopActive && event.Session == first.Session
	}, "the native Stop continuation in inventory")

	var stopID int64
	select {
	case stopID = <-stopAsk:
	default:
		t.Fatal("the inventory request did not arrive during the model turn")
	}
	pendingLines := regexp.MustCompile(`\[crosshall\] pending: ([0-9]+(?: [0-9]+)*)`)
	hasPending := func(request int, id int64) bool {
		for _, match := range pendingLines.FindAllStringSubmatch(c.model.request(request), -1) {
			if slices.Contains(strings.Fields(match[1]), strconv.FormatInt(id, 10)) {
				return true
			}
		}
		return false
	}
	if !hasPending(promptRequest, note) || hasPending(promptRequest, late) || hasPending(promptRequest+1, late) {
		t.Fatalf("inventory prompt or Stop carried the wrong work (inventory note %d, late checkout reply %d)", note, late)
	}
	if c.carries(promptRequest, stopID) != 0 || c.carries(promptRequest+1, stopID) != 1 {
		t.Fatalf("inventory request %d did not arrive once in the Stop continuation", stopID)
	}
	if count := c.storeCount("SELECT COUNT(*) FROM offer_item i JOIN offer o ON o.id = i.offer_id "+
		"WHERE i.message_id = ? AND o.binding_id = ? AND o.reason = 'prompt'", note, destination); count != 1 {
		t.Fatalf("inventory note %d was offered %d times by its prompt binding", note, count)
	}
	if count := c.storeCount("SELECT COUNT(*) FROM announcement a JOIN message m ON m.id = a.message_id "+
		"JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = a.principal_id "+
		"JOIN turn_end_notice n ON n.id = a.ref AND n.principal_id = a.principal_id "+
		"JOIN adapter_launch l ON l.principal_id = n.principal_id AND l.launch_ref = n.launch_ref "+
		"WHERE a.message_id = ? AND a.via = 'turn_end' AND b.id = ? AND l.rowid = ?", stopID, destination, launch); count != 1 {
		t.Fatalf("inventory Stop request %d has %d matching announcements", stopID, count)
	}
	if count := c.storeCount("SELECT COUNT(*) FROM recipient r JOIN message m ON m.id = r.message_id "+
		"JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = r.principal_id "+
		"WHERE r.message_id = ? AND b.id = ? AND r.picked_up_at_ms IS NULL "+
		"AND NOT EXISTS (SELECT 1 FROM offer_item i WHERE i.message_id = r.message_id AND i.principal_id = r.principal_id)",
		late, source); count != 1 {
		t.Fatalf("late checkout reply %d was offered or stopped being unread in checkout", late)
	}

	if verifyPublish {
		c.owner("owner", "publish", "hall", "on")
		waitForProof(destination, "a fresh inventory publish proof")
		term.idle()
		stops := c.count(named("Stop"))
		id := messageID(inventory("--json", "ask", "@moved", "Inventory native notice"))
		term.await("the Stop after the inventory native notice", func() bool { return c.count(named("Stop")) > stops })
		term.await("the inventory notice receipt", func() bool {
			return c.storeCount("SELECT COUNT(*) FROM publish_attempt_item i JOIN publish_attempt a ON a.id = i.attempt_id "+
				"JOIN adapter_launch l ON l.principal_id = a.principal_id AND l.launch_ref = a.launch_ref "+
				"WHERE i.message_id = ? AND a.binding_id = ? AND l.rowid = ? AND a.state = 'submitted' AND a.observation = 'own_turn'",
				id, destination, launch) == 1
		})
		if count := c.storeCount("SELECT COUNT(*) FROM publish_attempt_item WHERE message_id = ?", late); count != 0 {
			t.Fatalf("the source reply %d appeared in a native publish attempt", late)
		}
	}
	term.exit()

	for _, event := range c.events() {
		if event.Session != first.Session || (event.Process != "" && event.Process != first.Process) {
			t.Fatalf("the move changed the native callback identity: %+v", event)
		}
	}
	if count := c.storeCount("SELECT COUNT(*) FROM principal_key WHERE vendor = 'claude' AND key = ? AND state = 'live' AND principal_id = ?",
		first.Session, principal); count != 1 {
		t.Fatal("the original session key did not keep its principal")
	}
}
