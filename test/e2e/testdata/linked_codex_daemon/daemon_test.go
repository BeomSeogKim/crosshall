package linked_codex_daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/appserver/appservertest"
)

// The socket and hook children belong to this process so both production
// checks see the same managed server identity.
func TestManagedCodexDaemon(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "--managed-daemon" {
		t.Fatal("fixture was not started as the managed app server")
	}
	s := appservertest.Start(t)
	requests, err := os.OpenFile(os.Getenv("CROSSHALL_FIXTURE_CODEX_REQUESTS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer requests.Close()
	s.Answer = func(id int64, method string, params map[string]any) []byte {
		if method == "initialize" {
			return nil
		}
		entry, err := json.Marshal(map[string]any{"method": method, "params": params})
		if err != nil {
			return appservertest.Error(id, -32603, err.Error())
		}
		if _, err := requests.Write(append(entry, '\n')); err != nil {
			return appservertest.Error(id, -32603, err.Error())
		}
		switch method {
		case "thread/read":
			return appservertest.Result(id, map[string]any{"thread": map[string]any{"turns": []any{}}})
		case "thread/queue/add":
			return appservertest.Result(id, map[string]any{"queuedSubmission": map[string]string{"id": "fixture-submission"}})
		default:
			return appservertest.Error(id, -32601, "unexpected fixture method")
		}
	}
	socket := os.Getenv("CROSSHALL_FIXTURE_CODEX_CONTROL")
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.Socket, socket); err != nil {
		t.Fatal(err)
	}
	for stdin := bufio.NewScanner(os.Stdin); ; {
		if !stdin.Scan() {
			if err := stdin.Err(); err != nil {
				t.Fatal(err)
			}
			return
		}
		cmd := exec.CommandContext(t.Context(), os.Getenv("CROSSHALL_FIXTURE_CODEX_HOOK"), "hook", "codex", "SessionStart")
		cmd.Stdin = bytes.NewReader(stdin.Bytes())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("linked Codex SessionStart: %v: %s", err, out)
		}
	}
}
