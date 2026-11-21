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

func TestInstalledHookRepeatsUndeliveredJoinGuidance(t *testing.T) {
	for _, adapter := range []struct{ name, turn, identity string }{
		{"claude", "prompt_id", "CLAUDE_CODE_SESSION_ID"},
		{"codex", "turn_id", "CODEX_THREAD_ID"},
	} {
		t.Run(adapter.name, func(t *testing.T) {
			f := newSetupWorld(t)
			f.apply("", "setup", adapter.name, "--apply")
			repo := f.enrollmentRepo()
			db := f.db()
			prompt := func(turn string) string { return fmt.Sprintf(`%q:%q`, adapter.turn, turn) }

			f.blockHookOutput(repo, adapter.name, "SessionStart", `"source":"startup"`)
			if stage := f.enrollmentEvidence(adapter.name); !strings.Contains(stage, "live binding") {
				t.Fatalf("startup did not commit while stdout blocked: %s", stage)
			}
			f.blockHookOutput(repo, adapter.name, "UserPromptSubmit", prompt("blocked-again"))
			if roster, welcome := hookJoinAcknowledgements(t, db, adapter.name); roster || welcome {
				t.Fatal("the store acknowledged guidance that never reached stdout")
			}
			member := &participant{w: f.world, extra: []string{adapter.identity + "=" + setupSessionPrefix + adapter.name}}
			firstInbox := member.mustPlain("inbox")
			secondInbox := member.mustPlain("inbox")
			if !strings.Contains(firstInbox, "join setup-check --rules") || strings.Contains(secondInbox, "join setup-check --rules") ||
				strings.Contains(firstInbox+secondInbox, "Before voluntarily ending any turn") {
				t.Fatalf("unexpected inbox guidance: %q and %q", firstInbox, secondInbox)
			}
			if roster, welcome := hookJoinAcknowledgements(t, db, adapter.name); !roster || welcome {
				t.Fatalf("inbox should confirm only its roster: roster %t, welcome %t", roster, welcome)
			}
			out := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt("delivered"))
			if out == "" || !strings.Contains(hostContext(t, out), "Joined #setup-check as @"+adapter.name+"1") ||
				!strings.Contains(out, "Before voluntarily ending any turn") {
				t.Fatalf("submit hook did not deliver the welcome and rules: %q", out)
			}
			if got := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt("after-delivery")); got != "" {
				t.Fatalf("submit hook repeated delivered guidance: %q", got)
			}
			deadline := time.Now().Add(time.Second)
			for {
				if _, welcome := hookJoinAcknowledgements(t, db, adapter.name); welcome {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the store did not acknowledge delivered guidance")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
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

func hookJoinAcknowledgements(t *testing.T, db *sql.DB, adapter string) (roster, welcome bool) {
	t.Helper()
	if err := db.QueryRow(`SELECT b.hint_acked, b.roster_baseline_seq IS NULL
		FROM binding b JOIN principal_key k ON k.principal_id = b.principal_id
		WHERE k.vendor = ? AND k.key = ? AND b.state = 'live'`, adapter, setupSessionPrefix+adapter).Scan(&roster, &welcome); err != nil {
		t.Fatal(err)
	}
	return roster, welcome
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
