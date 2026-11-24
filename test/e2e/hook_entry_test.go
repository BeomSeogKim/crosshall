//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Cold launches may include OS verification before the hook clock starts.
const hookProcessTimeout = 3 * time.Second

const hookSession = "synthetic-hook-member"
const hookPrompt = `{"hook_event_name":"UserPromptSubmit","session_id":"synthetic-hook-member","turn_id":"turn-1","prompt":"secret prompt marker","transcript_path":"secret transcript marker"}`

// Exercise the installed entry shape with an isolated home and a dispatcher-shaped spool.
type hookWorld struct {
	*world
	leaf   string
	digest string
}

func newHookWorld(t *testing.T, member bool) *hookWorld {
	t.Helper()
	w := newWorld(t)
	digest := (ident.Identity{Vendor: "codex", Key: hookSession}).Digest()
	h := &hookWorld{world: w, leaf: filepath.Join(w.rendezvous(), "m", digest[:16]), digest: digest}
	dirs := []string{"first/in", "first/out", "m", "hooks"}
	if member {
		dirs = append(dirs, "m/"+digest[:16]+"/in", "m/"+digest[:16]+"/out")
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(w.rendezvous(), dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	h.write(filepath.Join(w.rendezvous(), "ready"), fmt.Sprintf(`{"v":1,"instance":"fixture-instance","pid":%d}`, os.Getpid()))
	h.write(filepath.Join(w.rendezvous(), "policy"), "codex 1 0 7\n")
	if member {
		h.write(filepath.Join(h.leaf, "gen"), "fixture-generation")
	}
	return h
}

func (h *hookWorld) write(path, content string) {
	h.t.Helper()
	if err := os.WriteFile(path+".part", []byte(content), 0600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(path+".part", path); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hookWorld) run(stdin io.Reader, serve func(), args ...string) string {
	h.t.Helper()
	var stdout bytes.Buffer
	h.runTo(hookProcessTimeout, stdin, &stdout, serve, args...)
	return stdout.String()
}

func (h *hookWorld) runTo(limit time.Duration, stdin io.Reader, stdout io.Writer, serve func(), args ...string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"hook"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
	cmd.Dir, cmd.Env = h.home, h.baseEnv()
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cancel()
			_ = cmd.Wait()
		}
	}()
	if serve != nil {
		serve()
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		h.t.Fatalf("hook %v exceeded %s: %v", args, limit, ctx.Err())
	}
	if err != nil || stderr.Len() != 0 {
		h.t.Fatalf("hook %v: %v, stderr %q", args, err, stderr.String())
	}
}

func (h *hookWorld) serve(dir string, result core.Result, beforeReply func()) []byte {
	h.t.Helper()
	in := filepath.Join(dir, "in")
	end := time.Now().Add(hookProcessTimeout)
	for {
		entries, err := os.ReadDir(in)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			attempt, ok := strings.CutSuffix(name, ".json")
			if !ok {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(in, name))
			if err != nil {
				h.t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(in, name)); err != nil {
				h.t.Fatal(err)
			}
			data, err := json.Marshal(wire.Response{Version: wire.Version, Attempt: attempt, Result: result})
			if err != nil {
				h.t.Fatal(err)
			}
			if beforeReply != nil {
				beforeReply()
			}
			h.write(filepath.Join(dir, "out", name), string(data))
			return raw
		}
		if time.Now().After(end) {
			h.t.Fatalf("no request in %s", in)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *hookWorld) assertSpoolsEmpty() {
	h.t.Helper()
	for _, dir := range []string{filepath.Join(h.rendezvous(), "first/in"), filepath.Join(h.leaf, "in")} {
		entries, err := os.ReadDir(dir)
		if !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
			h.t.Fatalf("unexpected spool contents in %s: %v, %v", dir, entries, err)
		}
	}
	if _, err := os.Stat(h.state); !os.IsNotExist(err) {
		h.t.Fatalf("hook created dispatcher state: %v", err)
	}
}

func (h *hookWorld) counterDropped() bool {
	h.t.Helper()
	logs, err := os.ReadDir(filepath.Join(h.rendezvous(), "hooks"))
	if err != nil {
		h.t.Fatal(err)
	}
	for _, log := range logs {
		if info, err := log.Info(); err != nil || info.Size() != 0 {
			return false
		}
	}
	return true
}

func (h *hookWorld) assertCounter(event, outcome string) time.Duration {
	h.t.Helper()
	dir := filepath.Join(h.rendezvous(), "hooks")
	logs, err := os.ReadDir(dir)
	if err != nil || len(logs) != 1 {
		h.t.Fatalf("expected one daily hook counter file, got %v, %v", logs, err)
	}
	if _, err := time.Parse("20060102", logs[0].Name()); err != nil {
		h.t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, logs[0].Name()))
	if err != nil {
		h.t.Fatal(err)
	}
	fields := strings.Fields(string(raw))
	if len(raw) > 64 || bytes.Count(raw, []byte("\n")) != 1 || !bytes.HasSuffix(raw, []byte("\n")) ||
		len(fields) != 4 || fields[0] != "codex" || fields[1] != event || fields[2] != outcome {
		h.t.Fatalf("unexpected hook counter record %q", raw)
	}
	ms, err := strconv.Atoi(fields[3])
	if err != nil || ms < 0 {
		h.t.Fatalf("invalid hook duration in %q", raw)
	}
	return time.Duration(ms) * time.Millisecond
}

func TestHookEntrySilentFailures(t *testing.T) {
	for _, tc := range []struct{ name, adapter, event, input string }{
		{"unknown-adapter", "unknown", "UserPromptSubmit", hookPrompt},
		{"unknown-event", "codex", "NoSuchEvent", hookPrompt},
		{"invalid-json", "codex", "UserPromptSubmit", `{"broken"`},
		{"event-mismatch", "codex", "Stop", hookPrompt},
		{"missing-identity", "codex", "UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","turn_id":"turn-1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookWorld(t, true)
			if out := h.run(strings.NewReader(tc.input), nil, tc.adapter, tc.event); out != "" {
				t.Fatalf("unexpected hook output %q", out)
			}
			h.assertSpoolsEmpty()
		})
	}
}

func TestHookEntryOpenStdinBounded(t *testing.T) {
	h := newHookWorld(t, true)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := w.WriteString("{"); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	start := time.Now()
	h.runTo(500*time.Millisecond, r, &stdout, nil, "codex", "UserPromptSubmit")
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("hook returned after %v without waiting for incomplete stdin", waited)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected hook output %q", stdout.String())
	}
	h.assertSpoolsEmpty()
	// The record shares the callback deadline, so a stall there drops it.
	if h.counterDropped() {
		t.Log("hook dropped its counter record at the deadline")
		return
	}
	if elapsed := h.assertCounter("UserPromptSubmit", "deadline"); elapsed < 100*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("unexpected elapsed time for incomplete stdin: %v", elapsed)
	}
}

func TestHookEntryPromptOffer(t *testing.T) {
	for _, policy := range []struct{ name, update string }{
		{"current", ""}, {"disabled", "codex 0 0 8\n"}, {"advanced-epoch", "codex 1 0 8\n"},
	} {
		t.Run(policy.name, func(t *testing.T) {
			h := newHookWorld(t, true)
			var packet []byte
			out := h.run(strings.NewReader(hookPrompt), func() {
				packet = h.serve(h.leaf, core.Result{Data: json.RawMessage(`{"epoch":7,"pending_line":"[crosshall] pending: 42"}`)}, func() {
					if policy.update != "" {
						h.write(filepath.Join(h.rendezvous(), "policy"), policy.update)
					}
				})
			}, "codex", "UserPromptSubmit")
			var req wire.Request
			if err := json.Unmarshal(packet, &req); err != nil {
				t.Fatal(err)
			}
			var args struct {
				Fact       string `json:"fact"`
				LaunchRef  string `json:"launch_ref"`
				EventRef   string `json:"event_ref"`
				TurnRef    string `json:"turn_ref"`
				WantOutput bool   `json:"want_output"`
				Epoch      int    `json:"epoch"`
			}
			if err := json.Unmarshal(req.Args, &args); err != nil {
				t.Fatal(err)
			}
			if req.Verb != "hook.event" || req.KeyDigest != h.digest || req.Generation != "fixture-generation" || req.Ack != 0 ||
				args.Fact != "prompt_submitted" || args.TurnRef != "turn-1" || !args.WantOutput || args.Epoch != 7 || args.EventRef == "" ||
				args.LaunchRef == "" || req.OpKey != "hook:codex:"+args.LaunchRef+":"+args.EventRef {
				t.Fatalf("unexpected hook request: %s", packet)
			}
			if window := req.ExpiresAtMS - req.IssuedAtMS; window <= 0 || window > 150 {
				t.Fatalf("hook spool attempt had a %d ms budget", window)
			}
			for _, marker := range []string{"secret prompt marker", "secret transcript marker"} {
				if bytes.Contains(packet, []byte(marker)) || strings.Contains(out, marker) {
					t.Fatal("private prompt or transcript path present in member spool or vendor output")
				}
			}
			if policy.update != "" {
				if out != "" {
					t.Fatalf("stale offer emitted after policy change: %q", out)
				}
				h.assertCounter("UserPromptSubmit", "stale_epoch")
				return
			}
			h.assertCounter("UserPromptSubmit", "rpc")
			var payload struct {
				HookSpecificOutput struct {
					HookEventName     string
					AdditionalContext string
				}
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil || payload.HookSpecificOutput.HookEventName != "UserPromptSubmit" ||
				payload.HookSpecificOutput.AdditionalContext != "[crosshall] pending: 42" {
				t.Fatalf("unexpected vendor payload %q, %v", out, err)
			}
		})
	}
}

func TestHookEntryClosedHostStdout(t *testing.T) {
	h := newHookWorld(t, true)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	h.runTo(hookProcessTimeout, strings.NewReader(hookPrompt), writer, func() {
		h.serve(h.leaf, core.Result{Data: json.RawMessage(`{"epoch":7,"pending_line":"[crosshall] pending: 42"}`)}, func() {
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}, "codex", "UserPromptSubmit")
}

func TestHookEntryStartLooksUpBeforeDefault(t *testing.T) {
	h := newHookWorld(t, false)
	var packet []byte
	input := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":"startup","cwd":%q}`, hookSession, h.home)
	out := h.run(strings.NewReader(input), func() {
		packet = h.serve(filepath.Join(h.rendezvous(), "first"), core.Result{Status: core.ExitNotEnrolled, Code: core.CodeNotEnrolled, Data: json.RawMessage(`{}`)}, nil)
	}, "codex", "SessionStart")
	var hello wire.Hello
	if err := json.Unmarshal(packet, &hello); err != nil || hello.Intent != "lookup" || hello.Vendor != "codex" || hello.Key != hookSession {
		t.Fatalf("unexpected first contact: %s, %v", packet, err)
	}
	var payload struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	hint := payload.HookSpecificOutput.AdditionalContext
	if payload.HookSpecificOutput.HookEventName != "SessionStart" || strings.Contains(hint, "\n") ||
		!strings.Contains(hint, "No channel is set for this project") || !strings.Contains(hint, "crosshall join <channel>") {
		t.Fatalf("unexpected startup guidance: %q", out)
	}
	if _, err := os.Stat(h.leaf); !os.IsNotExist(err) {
		t.Fatalf("unmatched start created a member leaf: %v", err)
	}
	h.assertSpoolsEmpty()
	h.assertCounter("SessionStart", "unmatched")
}

// The dispatcher-shaped spool lets the test take the publication lock after the
// request has arrived. The hook can finish writing stdout, but cannot enqueue
// its receipt for that write.
func TestHookEntryPrintedLastBatchWithoutReceipt(t *testing.T) {
	h := newHookWorld(t, true)
	h.write(filepath.Join(h.leaf, "stamp"), "fixture-instance fixture-generation 2 9\n")
	const line = "[crosshall] pending: 42"
	response := core.Result{Data: json.RawMessage(`{"epoch":7,"pending_line":"[crosshall] pending: 42","offer_receipt":{"offer_id":1,"nonce":"fixture"},"ack":{"kind":"notice","instance":"fixture-instance","generation":"fixture-generation","seq":2}}`)}
	lock, err := os.OpenFile(filepath.Join(h.leaf, "hook-publish.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	run := func(turn string, beforeReply func()) string {
		t.Helper()
		in := strings.Replace(hookPrompt, "turn-1", turn, 1)
		return hostContext(t, h.run(strings.NewReader(in), func() { h.serve(h.leaf, response, beforeReply) }, "codex", "UserPromptSubmit"))
	}
	if got := run("missed", func() {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
	}); got != line {
		t.Fatalf("while receipt publication was blocked got %q, want %q", got, line)
	}
	h.assertSpoolsEmpty()
	if _, err := os.Lstat(filepath.Join(h.leaf, "seen-notice")); !os.IsNotExist(err) {
		t.Fatalf("last batch without receipt left a clean stamp: %v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if got := run("retry", nil); got != line {
		t.Fatalf("retry after the lock was released got %q, want %q", got, line)
	}
	packets := fixtureQueuedPackets(t, h.leaf)
	if len(packets) != 1 {
		t.Fatalf("expected one receipt after the retry, got %v", packets)
	}
	raw, err := os.ReadFile(filepath.Join(h.leaf, packets[0]))
	if err != nil {
		t.Fatal(err)
	}
	var req wire.Request
	if err := json.Unmarshal(raw, &req); err != nil || req.Verb != wire.VerbHookJoinHintAck || !req.OneWayHook() {
		t.Fatalf("unexpected receipt packet %s (%v)", raw, err)
	}
	var ack wire.HookOutputAckArgs
	if err := json.Unmarshal(req.Args, &ack); err != nil || ack.Offer == nil || ack.Offer.OfferID != 1 || ack.Offer.Nonce != "fixture" || ack.WrittenAtMS <= 0 {
		t.Fatalf("receipt for retry %s (%v)", req.Args, err)
	}
}

func TestHookEntryCleanPromptPublishesWithoutWaiting(t *testing.T) {
	h := newHookWorld(t, true)
	h.write(filepath.Join(h.leaf, "stamp"), "fixture-instance fixture-generation 3 4\n")
	h.write(filepath.Join(h.leaf, "seen-notice"), "fixture-instance fixture-generation 3\n")
	h.write(filepath.Join(h.leaf, "ack"), "9\n")
	if out := h.run(strings.NewReader(hookPrompt), nil, "codex", "UserPromptSubmit"); out != "" {
		t.Fatalf("clean prompt emitted %q", out)
	}
	entries, err := os.ReadDir(filepath.Join(h.leaf, "in"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("wanted one observation, got %v, %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(h.leaf, "in", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var packet wire.Request
	if err := json.Unmarshal(raw, &packet); err != nil {
		t.Fatal(err)
	}
	var args wire.HookEventArgs
	if err := json.Unmarshal(packet.Args, &args); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(entries[0].Name(), ".json") || packet.Verb != wire.VerbHookEvent || packet.Reply == nil || *packet.Reply || args.WantOutput || args.Fact != wire.HookPromptSubmitted || packet.Ack != 0 {
		t.Fatalf("invalid one-way observation %s", raw)
	}
	ack, err := os.ReadFile(filepath.Join(h.leaf, "ack"))
	if err != nil || string(ack) != "9\n" {
		t.Fatalf("ordinary cursor changed: %q, %v", ack, err)
	}
	h.assertCounter("UserPromptSubmit", "clean")
}
