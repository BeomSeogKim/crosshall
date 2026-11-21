//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

// A hook tells a session it enrolled one line: who it is and who is there.
// The line is offered again until it reached stdout; the member rules come
// with the member's first command, and once a command gave them the hook has
// nothing left to say.
func TestInstalledHookRepeatsItsUndeliveredStartLine(t *testing.T) {
	for _, adapter := range []struct{ name, turn, identity string }{
		{"claude", "prompt_id", "CLAUDE_CODE_SESSION_ID"},
		{"codex", "turn_id", "CODEX_THREAD_ID"},
	} {
		prompt := func(turn string) string { return fmt.Sprintf(`%q:%q`, adapter.turn, turn) }
		const rules = "Before voluntarily ending any turn"
		line := "Joined #setup-check as @" + adapter.name + "1, with @peer."
		blocked := func(t *testing.T) (*setupWorld, string, *participant) {
			f := newSetupWorld(t)
			f.apply("", "setup", adapter.name, "--apply")
			repo := f.enrollmentRepo()
			f.blockHookOutput(repo, adapter.name, "SessionStart", `"source":"startup"`)
			if stage := f.enrollmentEvidence(adapter.name); !strings.Contains(stage, "live binding") {
				t.Fatalf("startup did not commit while stdout blocked: %s", stage)
			}
			f.blockHookOutput(repo, adapter.name, "UserPromptSubmit", prompt("blocked-again"))
			if command, hook := hookJoinAcknowledgements(t, f.db(), adapter.name); command || hook {
				t.Fatal("the store acknowledged a start line that never reached stdout")
			}
			return f, repo, &participant{w: f.world, extra: []string{adapter.identity + "=" + setupSessionPrefix + adapter.name}}
		}
		t.Run(adapter.name+" hook first", func(t *testing.T) {
			f, repo, member := blocked(t)
			out := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt("delivered"))
			if out == "" || !strings.Contains(hostContext(t, out), line) || strings.Contains(out, rules) {
				t.Fatalf("submit hook did not deliver the start line alone: %q", out)
			}
			if got := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt("after-delivery")); got != "" {
				t.Fatalf("submit hook repeated a delivered start line: %q", got)
			}
			deadline := time.Now().Add(time.Second)
			for {
				if _, hook := hookJoinAcknowledgements(t, f.db(), adapter.name); hook {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the store did not acknowledge the delivered start line")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// The rules come with the first command, once.
			firstInbox := member.mustPlain("inbox")
			secondInbox := member.mustPlain("inbox")
			if !strings.Contains(firstInbox, rules) || strings.Contains(secondInbox, rules) {
				t.Fatalf("unexpected inbox guidance: %q and %q", firstInbox, secondInbox)
			}
		})
		t.Run(adapter.name+" command first", func(t *testing.T) {
			f, repo, member := blocked(t)
			firstInbox := member.mustPlain("inbox")
			secondInbox := member.mustPlain("inbox")
			if !strings.Contains(firstInbox, "Joined #setup-check as @"+adapter.name+"1.") || !strings.Contains(firstInbox, rules) ||
				strings.Contains(secondInbox, rules) {
				t.Fatalf("unexpected inbox guidance: %q and %q", firstInbox, secondInbox)
			}
			if command, hook := hookJoinAcknowledgements(t, f.db(), adapter.name); !command || hook {
				t.Fatalf("inbox should confirm only its own guidance: command %t, hook %t", command, hook)
			}
			if got := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt("after-command")); got != "" {
				t.Fatalf("submit hook added a start line after the rules: %q", got)
			}
		})
		// The version before the start line left a roster cursor after a
		// reply that carried no rules. It outlives the upgrade and must not
		// settle the rules: the next command still prints them.
		for _, first := range [][]string{{"inbox"}, {"join", "setup-check"}} {
			t.Run(adapter.name+" an older version's cursor, then "+first[0], func(t *testing.T) {
				f, repo, member := blocked(t)
				digest := (ident.Identity{Vendor: adapter.name, Key: setupSessionPrefix + adapter.name}).Digest()
				if err := os.WriteFile(filepath.Join(f.rendezvous(), "m", digest[:16], "ack"), []byte("1\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if out := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt("upgraded")); !strings.Contains(hostContext(t, out), line) {
					t.Fatalf("the start line is still owed after the upgrade: %q", out)
				}
				once := member.mustPlain(first...)
				again := member.mustPlain(first...)
				if !strings.Contains(once, rules) || strings.Contains(again, rules) {
					t.Fatalf("unexpected %s guidance: %q and %q", first[0], once, again)
				}
			})
		}
		// One command gave the rules: the leaf says so before the store does,
		// and the next prompt must not add the start line.
		for _, first := range []string{"inbox", "whoami"} {
			t.Run(adapter.name+" one "+first+", then a prompt", func(t *testing.T) {
				f, repo, member := blocked(t)
				if out := member.mustPlain(first); !strings.Contains(out, rules) {
					t.Fatalf("the first %s carried no rules: %q", first, out)
				}
				if got := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt("after-one-command")); got != "" {
					t.Fatalf("submit hook added a start line after the rules: %q", got)
				}
				if command, hook := hookJoinAcknowledgements(t, f.db(), adapter.name); !command || hook {
					t.Fatalf("the prompt should record the rules the command printed: command %t, hook %t", command, hook)
				}
				if out := member.mustPlain("inbox"); strings.Contains(out, rules) {
					t.Fatalf("the rules were printed again: %q", out)
				}
			})
		}
	}
}

func TestHookJoinHintFilesRetireWithTheLeaf(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")
	repo := f.enrollmentRepo()
	if got := f.invokeHook(repo, "claude", "SessionStart", `"source":"startup"`); !strings.Contains(got, "Joined #setup-check as @claude1") {
		t.Fatalf("startup did not deliver a welcome: %q", got)
	}
	key := setupSessionPrefix + "claude"
	digest := (ident.Identity{Vendor: "claude", Key: key}).Digest()
	leaf := filepath.Join(f.rendezvous(), "m", digest[:16])
	guard := filepath.Join(leaf, "join-hint.lock")
	receipt := filepath.Join(leaf, "seen-join-hint")
	lock, err := os.OpenFile(guard, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	owner := f.owner()
	var bindingID int64
	if err := f.db().QueryRow(`SELECT b.id FROM binding b JOIN principal_key k ON k.principal_id = b.principal_id
		WHERE k.vendor = 'claude' AND k.key = ? AND b.state = 'live'`, key).Scan(&bindingID); err != nil {
		t.Fatal(err)
	}
	member := &participant{w: f.world, extra: []string{"CLAUDE_CODE_SESSION_ID=" + key}}
	member.must("leave")
	owner.must("owner", "restore", fmt.Sprint(bindingID), "--new-key", "claude:fixture-replacement-key")
	if old := member.run("inbox"); old.exit != 4 || old.obj["code"] != "key_retired" {
		t.Fatalf("the old leaf did not return its retired-key refusal: %d %v", old.exit, old.obj["code"])
	}
	for _, path := range []string{guard, receipt} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retired %s while the output lock was held: %v", filepath.Base(path), err)
		}
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, guardErr := os.Stat(guard)
		_, receiptErr := os.Stat(receipt)
		if os.IsNotExist(guardErr) && os.IsNotExist(receiptErr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fenced leaf retained its acknowledgement files: guard %v, receipt %v", guardErr, receiptErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hookJoinAcknowledgements reports what the store knows reached the session:
// the rules through a command, and the start line through a hook.
func hookJoinAcknowledgements(t *testing.T, db *sql.DB, adapter string) (command, hook bool) {
	t.Helper()
	if err := db.QueryRow(`SELECT b.hint_acked, b.start_line_seen
		FROM binding b JOIN principal_key k ON k.principal_id = b.principal_id
		WHERE k.vendor = ? AND k.key = ? AND b.state = 'live'`, adapter, setupSessionPrefix+adapter).Scan(&command, &hook); err != nil {
		t.Fatal(err)
	}
	return command, hook
}

func (f *setupWorld) blockHookOutput(repo, adapter, event, fields string) {
	f.t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		f.t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	fd := int(write.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		f.t.Fatal(err)
	}
	fill := bytes.Repeat([]byte{'x'}, 4096)
	filled := 0
	for len(fill) != 0 {
		n, err := syscall.Write(fd, fill)
		switch err {
		case nil:
			filled += n
		case syscall.EAGAIN:
			fill = fill[:len(fill)/2]
		case syscall.EINTR:
		default:
			f.t.Fatal(err)
		}
	}
	if err := syscall.SetNonblock(fd, false); err != nil || filled == 0 {
		f.t.Fatalf("fill the stdout pipe: %d bytes, %v", filled, err)
	}
	ctx, cancel := context.WithTimeout(f.t.Context(), 3*time.Second)
	defer cancel()
	cmd := f.installedHookCommand(ctx, repo, adapter, event, fields)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = write, &stderr
	if err := cmd.Run(); err != nil || stderr.Len() != 0 {
		f.t.Fatalf("invoke %s %s with blocked stdout: %v, stderr %q", adapter, event, err, stderr.String())
	}
	if err := write.Close(); err != nil {
		f.t.Fatal(err)
	}
	raw, err := io.ReadAll(read)
	if err != nil || !bytes.Equal(raw, bytes.Repeat([]byte{'x'}, filled)) {
		f.t.Fatalf("%s %s emitted unexpected stdout with its pipe full: %d bytes, %v", adapter, event, len(raw)-filled, err)
	}
}

// whoami asks for the roster but shows none of it: it must leave a peer's
// arrival for the next command to announce.
func TestWhoamiLeavesRosterNewsForTheNextCommand(t *testing.T) {
	w, _, c := m2World(t)
	c.fire("SessionStart", map[string]any{"source": "startup"})
	member := &participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + c.key}}
	member.mustPlain("inbox")
	member.mustPlain("inbox")
	(&participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=late-peer-session"}}).must("join", "checkout", "--as", "latepeer")
	if out := member.mustPlain("whoami"); strings.Contains(out, "latepeer") {
		t.Fatalf("whoami announced the roster: %q", out)
	}
	if out := member.mustPlain("inbox"); !strings.Contains(out, "@latepeer joined") {
		t.Fatalf("the peer's arrival was never announced: %q", out)
	}
}
