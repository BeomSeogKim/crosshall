//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstalledHookRepeatsUndeliveredJoinGuidance(t *testing.T) {
	for _, adapter := range []struct{ name, turn string }{{"claude", "prompt_id"}, {"codex", "turn_id"}} {
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
			if hookJoinAcknowledged(t, db, adapter.name) {
				t.Fatal("the store acknowledged guidance that never reached stdout")
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
			for !hookJoinAcknowledged(t, db, adapter.name) {
				if time.Now().After(deadline) {
					t.Fatal("the store did not acknowledge delivered guidance")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func hookJoinAcknowledged(t *testing.T, db *sql.DB, adapter string) bool {
	t.Helper()
	var acknowledged bool
	if err := db.QueryRow(`SELECT b.hint_acked FROM binding b JOIN principal_key k ON k.principal_id = b.principal_id
		WHERE k.vendor = ? AND k.key = ? AND b.state = 'live'`, adapter, setupSessionPrefix+adapter).Scan(&acknowledged); err != nil {
		t.Fatal(err)
	}
	return acknowledged
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
