//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// The issue #13 checks end to end: the real hook binary, the real dispatcher
// and the real domain, a synthetic Claude Code session in a temporary home.

type claudeSession struct {
	w        *world
	key, pid string
	repo     string
}

// fire runs `crosshall hook claude <event>` as an installed hook would and
// returns what it printed for the host.
func (c claudeSession) fire(event string, fields map[string]any) string {
	c.w.t.Helper()
	payload := map[string]any{"hook_event_name": event, "session_id": c.key, "cwd": c.repo}
	for k, v := range fields {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		c.w.t.Fatal(err)
	}
	cmd := exec.CommandContext(c.w.t.Context(), binary, "hook", "claude", event)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(raw), &stdout, &stderr
	cmd.Dir = c.repo
	cmd.Env = append(c.w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key, "CLAUDE_PID="+c.pid)
	if err := cmd.Run(); err != nil || stderr.Len() != 0 {
		c.w.t.Fatalf("hook %s: %v, stderr %q", event, err, stderr.String())
	}
	return stdout.String()
}

// waitForConsumption waits for terminal disposition of every publication from
// the hooks that have returned. A packet moves from in to work before handling
// and leaves work after handling. An expired, unclaimed packet can disappear
// from in without committing an observation.
func (c claudeSession) waitForConsumption(deadline time.Time) {
	c.w.t.Helper()
	digest := (ident.Identity{Vendor: "claude", Key: c.key}).Digest()
	rv := c.w.rendezvous()
	for {
		first := fixtureQueuedPackets(c.w.t, filepath.Join(rv, "first"))
		member := fixtureQueuedPackets(c.w.t, filepath.Join(rv, "m", digest[:16]))
		if len(first) == 0 && len(member) == 0 {
			return
		}
		if time.Now().After(deadline) {
			c.w.t.Fatalf("hook packets still queued: first contact %v, member %v", first, member)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c claudeSession) fireUntil(event string, fields map[string]any, expected string, observed func(string) bool) string {
	c.w.t.Helper()
	deadline := time.Now().Add(fixtureEventTimeout)
	for {
		out := c.fire(event, fields)
		c.waitForConsumption(deadline)
		if observed(out) {
			return out
		}
		if !time.Now().Before(deadline) {
			c.w.t.Fatalf("%s did not reach %s: last output %q, counter %q", event, expected, out, c.lastCounter())
		}
	}
}

func (c claudeSession) startWithContext(source string) string {
	c.w.t.Helper()
	out := c.fireUntil("SessionStart", map[string]any{"source": source}, source+" context", func(out string) bool {
		return out != ""
	})
	return hostContext(c.w.t, out)
}

// fireAndRecord waits for a new committed observation. A one-way packet can
// expire before admission, so retry after it leaves the queue if nothing committed.
func (c claudeSession) fireAndRecord(event, fact string, fields map[string]any) {
	c.w.t.Helper()
	digest := (ident.Identity{Vendor: "claude", Key: c.key}).Digest()
	// A prompt's event reference also carries its hook's entry time (#163):
	// match the turn and the kind.
	ref, _ := fields["prompt_id"].(string)
	db := c.w.db()
	count := func() int {
		c.w.t.Helper()
		var recorded int
		err := db.QueryRow(`SELECT COUNT(*) FROM observation o JOIN leaf l ON l.principal_id = o.principal_id
			WHERE l.digest = ? AND o.kind = ? AND (? = '' OR o.turn_ref = ?)`,
			digest, fact, ref, ref).Scan(&recorded)
		if err != nil {
			c.w.t.Fatal(err)
		}
		return recorded
	}
	before := count()
	deadline := time.Now().Add(fixtureEventTimeout)
	for {
		if out := c.fire(event, fields); out != "" {
			c.w.t.Fatalf("quiet %s printed %q", event, out)
		}
		c.waitForConsumption(deadline)
		if count() > before {
			return
		}
		if time.Now().After(deadline) {
			c.w.t.Fatalf("quiet %s (%q): still at %d observations, last counter %q", event, ref, before, c.lastCounter())
		}
	}
}

// lastCounter is "<event> <outcome>" of the most recent hook counter record.
func (c claudeSession) lastCounter() string {
	c.w.t.Helper()
	dir := filepath.Join(c.w.rendezvous(), "hooks")
	logs, err := os.ReadDir(dir)
	if err != nil || len(logs) == 0 {
		c.w.t.Fatalf("no hook counters: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, logs[len(logs)-1].Name()))
	if err != nil {
		c.w.t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) != 4 {
		c.w.t.Fatalf("counter record %q", lines[len(lines)-1])
	}
	return f[1] + " " + f[2]
}

// hostContext is the additionalContext a hook printed, or "" for silence.
func hostContext(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var v struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Output.Context == "" {
		t.Fatalf("hook output is not one context object: %q (%v)", out, err)
	}
	return v.Output.Context
}

func m2World(t *testing.T) (*world, *participant, claudeSession) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	repo := filepath.Join(w.home, "checkout")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	owner.must("owner", "adapter", "set", "claude", "--enabled", "on")
	writer := w.member()
	writer.must("join", "checkout", "--as", "writer")
	return w, writer, claudeSession{w: w, key: "synthetic-m2-session", pid: strconv.Itoa(os.Getpid()), repo: repo}
}

func notes(t *testing.T, from *participant, alias string, n int) []string {
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, strconv.FormatInt(num(t, from.must("note", "@"+alias, "synthetic note "+strconv.Itoa(i)), "id"), 10))
	}
	return ids
}

func pendingContext(ids []string) string { return "[crosshall] pending: " + strings.Join(ids, " ") }

func rosterHas(t *testing.T, viewer *participant, alias string) bool {
	for _, a := range viewer.must("who").aliases() {
		if a == alias {
			return true
		}
	}
	return false
}

func tableCounts(t *testing.T, w *world) map[string]int {
	t.Helper()
	db := w.db()
	out := map[string]int{}
	for _, table := range []string{"observation", "offer", "offer_item", "roster_change", "adapter_launch", "turn", "binding", "event"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func TestM2SessionThroughTheHookBinary(t *testing.T) {
	w, writer, c := m2World(t)

	if got := hostContext(t, c.fire("SessionStart", map[string]any{"source": "startup"})); !strings.Contains(got, "Joined #checkout as @claude1") {
		t.Fatalf("auto-join context %q", got)
	}
	mail := notes(t, writer, "claude1", 2)
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p1", "prompt": "synthetic"})); got != pendingContext(mail) {
		t.Fatalf("first prompt %q", got)
	}
	// Mail during a turn is offered at the next prompt even though a stop came first.
	late := notes(t, writer, "claude1", 1)
	c.fireAndRecord("Stop", wire.HookTurnFinishing, map[string]any{"prompt_id": "p1", "stop_hook_active": false})
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p2"})); got != pendingContext(late) {
		t.Fatalf("prompt after stop %q", got)
	}
	// Eleven unread across two prompts, none lost.
	eleven := notes(t, writer, "claude1", 11)
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p3"})); got != pendingContext(eleven[:10]) {
		t.Fatalf("ten oldest %q", got)
	}
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p4"})); got != pendingContext(eleven[10:]) {
		t.Fatalf("the eleventh %q", got)
	}
	// The clean stamp and the quiet Stop both publish without a round trip.
	c.fireAndRecord("UserPromptSubmit", wire.HookPromptSubmitted, map[string]any{"prompt_id": "p5"})
	c.fireAndRecord("Stop", wire.HookTurnFinishing, map[string]any{"prompt_id": "p5", "stop_hook_active": false})
	// A second replay of every callback that carries a native reference
	// prints nothing and changes nothing but the record of the replayed
	// prompts, whose references carry their own hook time (#174: the turns
	// are placed from every recorded prompt). (Starts and ends carry none:
	// the hook's own time tells them apart, so a replay of one is a new
	// event.)
	before := tableCounts(t, w)
	before["observation"] += 5
	for _, p := range []string{"p1", "p2", "p3", "p4", "p5"} {
		if got := c.fire("UserPromptSubmit", map[string]any{"prompt_id": p}); got != "" {
			t.Fatalf("replayed prompt %s printed %q", p, got)
		}
	}
	for _, p := range []string{"p1", "p5"} {
		if got := c.fire("Stop", map[string]any{"prompt_id": p, "stop_hook_active": false}); got != "" {
			t.Fatalf("replayed stop %s printed %q", p, got)
		}
	}
	c.waitForConsumption(time.Now().Add(fixtureEventTimeout))
	if after := tableCounts(t, w); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed rows: %v -> %v", before, after)
	}

	// A clean end of the bound launch departs; the next start restores.
	c.fireAndRecord("SessionEnd", wire.HookSessionEnded, map[string]any{"reason": "other"})
	if rosterHas(t, writer, "claude1") {
		t.Fatal("a clean end did not depart the member")
	}
	c.fire("SessionStart", map[string]any{"source": "resume"})
	if !rosterHas(t, writer, "claude1") {
		t.Fatal("the next start did not restore the member")
	}
}

func TestM2TypedPublishNoticeIsObservationOnly(t *testing.T) {
	w, writer, c := m2World(t)
	if got := hostContext(t, c.fire("SessionStart", map[string]any{"source": "startup"})); !strings.Contains(got, "Joined #checkout as @claude1") {
		t.Fatalf("auto-join context %q", got)
	}
	grantRoots(t, w, "writer", 1)
	id := num(t, writer.must("ask", "@claude1", "a request stays unread"), "id")
	pending := pendingContext([]string{strconv.FormatInt(id, 10)})
	notice := pending + ". This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall read."
	fields := map[string]any{"prompt_id": "typed-notice", "prompt": notice}
	if got := hostContext(t, c.fire("UserPromptSubmit", fields)); got != pending {
		t.Fatalf("typed notice changed ordinary prompt context: %q", got)
	}
	db := w.db()
	assertWorkUnread(t, db, id)
	if got := c.fire("UserPromptSubmit", fields); got != "" {
		t.Fatalf("duplicate typed prompt produced output: %q", got)
	}
	c.waitForConsumption(time.Now().Add(fixtureEventTimeout))
	assertWorkUnread(t, db, id)
	var observations int
	// A second callback with the same prompt id is a prompt typed while the
	// turn ran (#163): it is observed too, and repeats no ID.
	if err := db.QueryRow(`SELECT COUNT(*) FROM observation WHERE turn_ref = ? AND kind = ?`,
		"typed-notice", wire.HookPromptSubmitted).Scan(&observations); err != nil || observations != 2 {
		t.Fatalf("typed notice observations = %d, error %v", observations, err)
	}
}

func TestM2HookFailuresPrintNothingAndAreCounted(t *testing.T) {
	w, _, c := m2World(t)
	c.fire("SessionStart", map[string]any{"source": "startup"})

	// Conflicting identity: the environment names another session.
	other := c
	other.key = "synthetic-other"
	cmd := exec.CommandContext(t.Context(), binary, "hook", "claude", "UserPromptSubmit")
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"synthetic-m2-session","prompt_id":"x"}`)
	cmd.Dir, cmd.Env = c.repo, append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID=synthetic-other", "CLAUDE_PID="+c.pid)
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("conflicting identity: %q, %v", out, err)
	}
	// Malformed input.
	cmd = exec.CommandContext(t.Context(), binary, "hook", "claude", "UserPromptSubmit")
	cmd.Stdin = strings.NewReader(`{"broken"`)
	cmd.Dir, cmd.Env = c.repo, append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key, "CLAUDE_PID="+c.pid)
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("malformed input: %q, %v", out, err)
	}
	// Switched off.
	w.owner().must("owner", "adapter", "set", "claude", "--enabled", "off")
	if got := c.fire("UserPromptSubmit", map[string]any{"prompt_id": "off-1"}); got != "" {
		t.Fatalf("switched-off adapter printed %q", got)
	}
	readCounters := func() string {
		t.Helper()
		dir := filepath.Join(w.rendezvous(), "hooks")
		logs, err := os.ReadDir(dir)
		if err != nil || len(logs) == 0 {
			t.Fatalf("no hook counters: %v", err)
		}
		var counters strings.Builder
		for _, log := range logs {
			raw, err := os.ReadFile(filepath.Join(dir, log.Name()))
			if err != nil {
				t.Fatal(err)
			}
			counters.Write(raw)
		}
		return counters.String()
	}
	before := readCounters()
	for _, outcome := range []string{" identity ", " refused ", " off "} {
		if !strings.Contains(before, outcome) {
			t.Errorf("counter outcome %q missing from\n%s", strings.TrimSpace(outcome), before)
		}
	}

	// The prompt may reach the new dispatcher before its callback deadline.
	w.owner().must("owner", "adapter", "set", "claude", "--enabled", "on")
	w.kill()
	out := c.fire("UserPromptSubmit", map[string]any{"prompt_id": "dead-1"})
	raw := readCounters()
	added, appended := strings.CutPrefix(raw, before)
	last := strings.Fields(added)
	if !appended || len(last) != 4 || last[0] != "claude" || last[1] != "UserPromptSubmit" {
		t.Fatalf("the restart prompt did not append one counter: %q", added)
	}
	switch last[2] {
	case "no_dispatcher", "refused":
		// The adapter policy can be replaced while the hook opens it at restart.
		if out != "" {
			t.Errorf("restart prompt counted as %s printed %q", last[2], out)
		}
	case "rpc":
		const welcome = "[crosshall] Joined #checkout as @claude1, with @writer. `crosshall join checkout --rules` prints the member rules."
		if got := hostContext(t, out); got != "" && got != welcome {
			t.Errorf("restart prompt context %q, want silence or %q", got, welcome)
		}
	default:
		t.Errorf("unexpected restart prompt counter: %v", last)
	}
	if ms, err := strconv.Atoi(last[3]); err != nil || ms < 0 || ms >= int(hook.Budget.Milliseconds()) {
		t.Errorf("restart prompt counter has elapsed time %q, want less than %d ms", last[3], hook.Budget.Milliseconds())
	}
	for deadline := time.Now().Add(fixtureEventTimeout); len(w.dispatcherPIDs()) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hook did not launch a dispatcher")
		}
	}
	if strings.Contains(raw, c.key) || strings.Contains(raw, "synthetic note") {
		t.Fatal("a counter record carries an identity or content")
	}
}

// Issue #100: the same session resumed in a second process. The end of one
// process keeps the member while the other still runs; the end of the last
// one departs it.
func TestM2EndOfOneOfTwoProcessesKeepsTheMember(t *testing.T) {
	_, writer, c := m2World(t)
	process := func() *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	first, second := process(), process()
	a, b := c, c
	a.pid, b.pid = strconv.Itoa(first.Process.Pid), strconv.Itoa(second.Process.Pid)
	a.fire("SessionStart", map[string]any{"source": "startup"})
	b.fire("SessionStart", map[string]any{"source": "resume"})
	if !rosterHas(t, writer, "claude1") {
		t.Fatal("the session did not join")
	}

	b.fireAndRecord("SessionEnd", wire.HookSessionEnded, map[string]any{"reason": "other"})
	if !rosterHas(t, writer, "claude1") {
		t.Fatal("the end of the resumed copy departed the member while the first process runs")
	}

	_ = first.Process.Kill()
	_ = first.Wait()
	a.fireAndRecord("SessionEnd", wire.HookSessionEnded, map[string]any{"reason": "other"})
	if rosterHas(t, writer, "claude1") {
		t.Fatal("the end of the last process did not depart the member")
	}
}

// Issue #100 review: overlapping and killed launches of one session, with
// real processes. Whatever the order, the end of the last running process
// departs the member.
func TestM2OverlappingLaunchEndsDepartAfterTheLast(t *testing.T) {
	for _, which := range []string{"latest-sigkill", "overlapping-clean-ends", "same-pid-open-row"} {
		t.Run(which, func(t *testing.T) {
			w, writer, c := m2World(t)
			spawn := func() *exec.Cmd {
				process := exec.Command("/bin/sleep", "60")
				if err := process.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
				return process
			}
			a, b := c, c
			first, second := spawn(), spawn()
			a.pid = strconv.Itoa(first.Process.Pid)
			b.pid = strconv.Itoa(second.Process.Pid)
			db := w.db()
			register := func(s claudeSession, source string, want int) {
				t.Helper()
				s.fireUntil("SessionStart", map[string]any{"source": source}, strconv.Itoa(want)+" registered launches", func(string) bool {
					var got int
					if err := db.QueryRow(`SELECT COUNT(*) FROM adapter_launch`).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got > want {
						t.Fatalf("%s registered %d launches, want %d", source, got, want)
					}
					return got == want
				})
			}
			register(a, "startup", 1)
			register(b, "resume", 2)
			end := func(s claudeSession) {
				before := tableCounts(t, w)["observation"]
				s.fireAndRecord("SessionEnd", wire.HookSessionEnded, map[string]any{"reason": "other"})
				if got := tableCounts(t, w)["observation"]; got != before+1 {
					t.Fatalf("SessionEnd observation count %d, want %d", got, before+1)
				}
			}
			switch which {
			case "latest-sigkill":
				if err := second.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := second.Wait(); err == nil {
					t.Fatal("expected SIGKILL wait result")
				}
				end(a)
			case "overlapping-clean-ends":
				end(a)
				if !rosterHas(t, writer, "claude1") {
					t.Fatal("A end departed while B is open")
				}
				end(b)
			case "same-pid-open-row":
				register(b, "clear", 3)
				end(b)
				if !rosterHas(t, writer, "claude1") {
					t.Fatal("B end departed while A is open")
				}
				if err := second.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = second.Wait()
				end(a)
			}
			if rosterHas(t, writer, "claude1") {
				t.Fatal("member remains live after its last launch ended")
			}
		})
	}
}

// A compaction or a clear resets the context but not the membership. The
// hook lists nothing the session was already offered: it gives one count of
// the work still waiting, and after a clear also who the session is and the
// command that prints the rules.
func TestM2ContextResetGetsACueNotARepeat(t *testing.T) {
	w, writer, c := m2World(t)
	if got := c.startWithContext("startup"); !strings.Contains(got, "Joined #checkout as @claude1") {
		t.Fatalf("auto-join context %q", got)
	}
	const pointer = "[crosshall] You are @claude1 in #checkout. `crosshall join checkout --rules` prints the member rules."
	if got := c.startWithContext("clear"); got != pointer {
		t.Fatalf("clear with nothing waiting: %q", got)
	}
	db := w.db()
	latestStart := func() int64 {
		t.Helper()
		var started int64
		if err := db.QueryRow(`SELECT MAX(started_hook_at_ms) FROM adapter_launch`).Scan(&started); err != nil {
			t.Fatal(err)
		}
		return started
	}
	before := latestStart()
	c.fireUntil("SessionStart", map[string]any{"source": "compact"}, "a committed compact", func(out string) bool {
		if out != "" {
			t.Fatalf("compact with nothing waiting: %q", out)
		}
		return latestStart() > before
	})
	grantRoots(t, w, "writer", 1)
	id := num(t, writer.must("ask", "@claude1", "a request offered before the reset"), "id")
	pending := pendingContext([]string{strconv.FormatInt(id, 10)})
	// An offer can commit even if the hook has no time left to print it.
	c.fireUntil("UserPromptSubmit", map[string]any{"prompt_id": "before-reset", "prompt": "go on"}, "a committed prompt offer", func(out string) bool {
		if out != "" && hostContext(t, out) != pending {
			t.Fatalf("prompt offer %q", out)
		}
		var offered bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM offer_item i JOIN offer o ON o.id = i.offer_id
			WHERE i.message_id = ? AND o.reason = 'prompt')`, id).Scan(&offered); err != nil {
			t.Fatal(err)
		}
		return offered
	})
	const count = "[crosshall] 1 unread, 0 owed. Run crosshall read."
	if got := c.startWithContext("compact"); got != count {
		t.Fatalf("compact with an offered ask unread: %q", got)
	}
	if got := c.startWithContext("clear"); got != pointer+"\n"+count {
		t.Fatalf("clear with an offered ask unread: %q", got)
	}
	assertWorkUnread(t, db, id)
}
