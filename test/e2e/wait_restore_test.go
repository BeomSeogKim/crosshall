//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestWaitEndsWhenRestoredStoreChangesItsCursor(t *testing.T) {
	for padding, name := range []string{"lower", "reused", "higher"} {
		t.Run(name, func(t *testing.T) {
			testWaitAcrossStoreRestore(t, padding)
		})
	}
}

func testWaitAcrossStoreRestore(t *testing.T, padding int) {
	t.Helper()
	w, owner, members := setup(t)
	form, tax := members[0], members[1]
	tax.must("note", "@lint", "before the backup")
	db := w.db()
	snapshot := filepath.Join(t.TempDir(), "older.sqlite3")
	if _, err := db.Exec("VACUUM INTO ?", snapshot); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tax.must("note", "@lint", "after the backup")
	last := num(t, tax.must("note", "@lint", "the wait's high-water mark"), "id")
	process, child := startWaitProcessAfterInitialQuery(t, form)
	if err := process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Signal(syscall.SIGCONT) }()
	owner.must("owner", "stop")
	store := filepath.Join(w.state, "store.sqlite3")
	if err := os.Rename(store, filepath.Join(w.state, "before-restore.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snapshot, store); err != nil {
		t.Fatal(err)
	}
	tax.must("who")
	if padding > 0 {
		tax.must("note", "@lint", "before the matched note in the restored store")
	}
	message := num(t, tax.must("note", "@form", "first match after restore"), "id")
	newMax := message
	if padding > 1 {
		newMax = num(t, tax.must("note", "@lint", "after the matched note in the restored store"), "id")
	}
	if difference := newMax - last; difference != int64(padding)-1 {
		t.Fatalf("restored maximum ID %d, cursor %d, expected offset %d", newMax, last, padding-1)
	}
	if err := process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-child:
		var failure struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		}
		exit, _ := got.err.(*exec.ExitError)
		if err := json.Unmarshal([]byte(got.stdout), &failure); err != nil || exit == nil ||
			exit.ExitCode() != 3 || failure.Code != "wait_cursor_invalidated" || failure.Hint != "crosshall inbox" {
			t.Fatalf("wait returned %v, stdout %s, stderr %s", got.err, got.stdout, got.stderr)
		}
	case <-time.After(fixtureEventTimeout):
		t.Fatal("wait did not report the restored message history")
	}
	if got := ids(form.must("inbox").obj["unread"]); !slices.Equal(got, []int64{message}) {
		t.Fatalf("inbox has %v, want restored message %d", got, message)
	}
}
