//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// macOS's daily /tmp cleaner can remove the snapshots hooks read while the
// dispatcher keeps running (#378). Without its policy every hook is off.
// The dispatcher republishes a removed snapshot, and the next prompt again
// delivers the session's notice and is recorded.
func TestHooksAnswerAsBeforeOnceARemovedSnapshotIsBack(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")
	repo := f.enrollmentRepo()
	writer := f.member()
	writer.must("join", "setup-check", "--as", "writer")
	f.waitForEnrollmentWelcome(repo, "claude", "SessionStart", func(string) string { return `"source":"startup"` })
	db := f.db()
	for _, name := range []string{"policy", "repo-defaults", "turn_end", "builds"} {
		path := filepath.Join(f.rendezvous(), name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(path); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) || !time.Now().Before(deadline) {
				t.Fatalf("%s was not republished: %v", name, err)
			}
		}
		notes(t, writer, "claude1", 1)
		turn := "after-" + name
		out := f.invokeHook(repo, "claude", "UserPromptSubmit", fmt.Sprintf(`"prompt_id":%q,"turn_id":%q,"prompt":"x"`, turn, turn))
		if hostContext(t, out) == "" {
			t.Fatalf("after %s was republished the prompt delivered no notice: %q", name, out)
		}
		for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(20 * time.Millisecond) {
			var observed int
			if err := db.QueryRow(`SELECT COUNT(*) FROM observation WHERE turn_ref = ? AND kind = 'prompt_submitted'`, turn).
				Scan(&observed); err == nil && observed > 0 {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("after %s was republished the prompt was not recorded", name)
			}
		}
		f.invokeHook(repo, "claude", "Stop", fmt.Sprintf(`"turn_id":%q,"stop_hook_active":false`, turn))
	}
}
