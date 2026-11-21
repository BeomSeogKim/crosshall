//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

			key := setupSessionPrefix + adapter.name
			for deadline := time.Now().Add(fixtureEventTimeout); ; {
				f.blockHookOutput(repo, adapter.name, "SessionStart", `"source":"startup"`)
				f.waitEnrollmentPackets(adapter.name, "SessionStart", deadline)
				var live bool
				if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM binding b JOIN principal_key k ON k.principal_id = b.principal_id
					WHERE k.vendor = ? AND k.key = ? AND b.state = 'live')`, adapter.name, key).Scan(&live); err != nil {
					t.Fatal(err)
				}
				if live {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("startup did not commit while stdout blocked: %s", f.enrollmentEvidence(adapter.name))
				}
			}
			for deadline, attempt := time.Now().Add(fixtureEventTimeout), 1; ; attempt++ {
				turn := fmt.Sprintf("blocked-%d", attempt)
				f.blockHookOutput(repo, adapter.name, "UserPromptSubmit", prompt(turn))
				f.waitEnrollmentPackets(adapter.name, "UserPromptSubmit", deadline)
				if hookPromptObserved(t, db, adapter.name, turn) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("blocked prompt was never observed; last hook %q", f.hookCounter(adapter.name, "UserPromptSubmit"))
				}
			}
			if roster, welcome := hookJoinAcknowledgements(t, db, adapter.name); roster || welcome {
				t.Fatal("the store acknowledged guidance that never reached stdout")
			}
			receipt := filepath.Join(f.enrollmentLeaf(adapter.name), "seen-join-hint")
			if _, err := os.Lstat(receipt); !os.IsNotExist(err) {
				t.Fatalf("blocked guidance left a delivery receipt: %v", err)
			}
			member := &participant{w: f.world, extra: []string{adapter.identity + "=" + key}}
			firstInbox := member.mustPlain("inbox")
			secondInbox := member.mustPlain("inbox")
			if !strings.Contains(firstInbox, "join setup-check --rules") || strings.Contains(secondInbox, "join setup-check --rules") ||
				strings.Contains(firstInbox+secondInbox, "Before voluntarily ending any turn") {
				t.Fatalf("unexpected inbox guidance: %q and %q", firstInbox, secondInbox)
			}
			if roster, welcome := hookJoinAcknowledgements(t, db, adapter.name); !roster || welcome {
				t.Fatalf("inbox should confirm only its roster: roster %t, welcome %t", roster, welcome)
			}
			deadline := time.Now().Add(fixtureEventTimeout)
			f.waitForWelcomeDelivery(repo, adapter.name, "UserPromptSubmit", prompt, deadline)
			f.waitEnrollmentPackets(adapter.name, "UserPromptSubmit", deadline)
			if info, err := os.Lstat(receipt); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("native guidance did not leave a delivery receipt: %v", err)
			}
			for attempt := 1; ; attempt++ {
				turn := fmt.Sprintf("after-delivery-%d", attempt)
				if got := f.invokeHook(repo, adapter.name, "UserPromptSubmit", prompt(turn)); got != "" {
					t.Fatalf("submit hook repeated delivered guidance: %q", got)
				}
				f.waitEnrollmentPackets(adapter.name, "UserPromptSubmit", deadline)
				if hookPromptObserved(t, db, adapter.name, turn) {
					if _, welcome := hookJoinAcknowledgements(t, db, adapter.name); !welcome {
						t.Fatal("the store did not acknowledge delivered guidance")
					}
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatalf("quiet prompt after delivery was never observed; last hook %q", f.hookCounter(adapter.name, "UserPromptSubmit"))
				}
			}
		})
	}
}

func TestHookJoinHintFilesRetireWithTheLeaf(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")
	repo := f.enrollmentRepo()
	f.waitForEnrollmentWelcome(repo, "claude", "SessionStart", func(string) string { return `"source":"startup"` })
	key := setupSessionPrefix + "claude"
	leaf := f.enrollmentLeaf("claude")
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
	deadline := time.Now().Add(fixtureEventTimeout)
	for {
		_, guardErr := os.Stat(guard)
		_, receiptErr := os.Stat(receipt)
		if os.IsNotExist(guardErr) && os.IsNotExist(receiptErr) {
			return
		}
		if (guardErr != nil && !os.IsNotExist(guardErr)) || (receiptErr != nil && !os.IsNotExist(receiptErr)) || !time.Now().Before(deadline) {
			f.logDispatcher()
			t.Fatalf("fenced leaf retained its acknowledgement files: guard %v, receipt %v", guardErr, receiptErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hookPromptObserved(t *testing.T, db *sql.DB, adapter, turn string) bool {
	t.Helper()
	var observed bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM observation o JOIN principal_key k ON k.principal_id = o.principal_id
		WHERE k.vendor = ? AND k.key = ? AND o.kind = 'prompt_submitted' AND o.event_ref = ?)`,
		adapter, setupSessionPrefix+adapter, turn+":UserPromptSubmit").Scan(&observed); err != nil {
		t.Fatal(err)
	}
	return observed
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
	ctx, cancel := context.WithTimeout(f.t.Context(), 3*time.Second)
	defer cancel()
	blockHookProcess(f.t, f.installedHookCommand(ctx, repo, adapter, event, fields))
}

func blockHookProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	fd := int(write.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
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
			t.Fatal(err)
		}
	}
	if err := syscall.SetNonblock(fd, false); err != nil || filled == 0 {
		t.Fatalf("fill the stdout pipe: %d bytes, %v", filled, err)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = write, &stderr
	if err := cmd.Run(); err != nil || stderr.Len() != 0 {
		t.Fatalf("invoke %v with blocked stdout: %v, stderr %q", cmd.Args, err, stderr.String())
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(read)
	if err != nil || !bytes.Equal(raw, bytes.Repeat([]byte{'x'}, filled)) {
		t.Fatalf("%v emitted unexpected stdout with its pipe full: %d bytes, %v", cmd.Args, len(raw)-filled, err)
	}
}
