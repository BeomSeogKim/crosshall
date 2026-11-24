package publish

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/appserver/appservertest"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// codexSendWorld is a home whose control socket links to a stand-in app
// server run by this test process, as Codex links it into its own runtime
// directory.
func codexSendWorld(t *testing.T) (*appservertest.Server, Target) {
	t.Helper()
	s := appservertest.Start(t)
	home, err := os.MkdirTemp("", "ch-cx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "app-server-control"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.Socket, filepath.Join(home, codexControlSocket)); err != nil {
		t.Fatal(err)
	}
	start, err := proc.StartOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return s, Target{Adapter: "codex", Address: "thread-1", Home: home, Attempt: "crosshall-7-1795",
		Queue: Queue{PID: int64(os.Getpid()), StartMS: start.Identity}}
}

func sendWithin(t *testing.T, d time.Duration, target Target, line string) Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return sendCodex(ctx, target, line)
}

// A notice is queued on its thread, tagged with its attempt, with
// thread/queue/add and nothing else: never turn/start, which would steer a
// working turn.
func TestCodexSendQueuesTheNoticeOnItsThread(t *testing.T) {
	s, target := codexSendWorld(t)
	var params map[string]any
	s.Answer = func(id int64, method string, p map[string]any) []byte {
		if method != "thread/queue/add" {
			return nil
		}
		params = p
		return appservertest.Result(id, map[string]any{"queuedSubmission": map[string]any{"id": "sub-1"}})
	}
	if got := sendWithin(t, time.Second, target, "[crosshall] pending: 3."); got != Submitted {
		t.Fatalf("outcome %s, want submitted", got)
	}
	if got := s.Methods(); !slices.Equal(got, []string{"initialize", "initialized", "thread/queue/add"}) {
		t.Fatalf("sent %v", got)
	}
	input, _ := params["input"].([]any)
	item, _ := input[0].(map[string]any)
	if params["threadId"] != "thread-1" || params["clientUserMessageId"] != "crosshall-7-1795" || len(input) != 1 ||
		item["type"] != "text" || item["text"] != "[crosshall] pending: 3." {
		t.Fatalf("queue/add params %v", params)
	}
}

// The server's error answer is a refusal: nothing was queued.
func TestCodexSendRefusedByTheServerIsNotSubmitted(t *testing.T) {
	s, target := codexSendWorld(t)
	s.Answer = func(id int64, method string, _ map[string]any) []byte {
		if method == "thread/queue/add" {
			return appservertest.Error(id, -32603, "no rollout found for thread id thread-1")
		}
		return nil
	}
	if got := sendWithin(t, time.Second, target, "n"); got != NotSubmitted {
		t.Fatalf("outcome %s, want not_submitted", got)
	}
}

// Without the proven server on the socket, or without a home, thread or
// attempt, nothing is written.
func TestCodexSendWritesNothingWithoutItsTarget(t *testing.T) {
	s, target := codexSendWorld(t)
	other := target
	other.Queue.StartMS++
	noHome, relHome, noThread, noAttempt := target, target, target, target
	noHome.Home, relHome.Home, noThread.Address, noAttempt.Attempt = "", "home", "", ""
	missing := target
	missing.Home = t.TempDir()
	for name, tg := range map[string]Target{"another server": other, "no home": noHome, "relative home": relHome,
		"no thread": noThread, "no attempt": noAttempt, "no socket": missing} {
		if got := sendWithin(t, time.Second, tg, "n"); got != NotSubmitted {
			t.Errorf("%s: outcome %s, want not_submitted", name, got)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := s.Methods(); len(got) != 0 {
		t.Fatalf("wrote %v", got)
	}
}

// Once the call is written, no answer by the deadline, or an answer that
// names no submission, may have queued it.
func TestCodexSendWithoutAClearAnswerIsUncertain(t *testing.T) {
	s, target := codexSendWorld(t)
	s.Answer = func(id int64, method string, p map[string]any) []byte {
		if method != "thread/queue/add" {
			return nil
		}
		if p["clientUserMessageId"] == "slow" {
			time.Sleep(time.Second)
		}
		return appservertest.Result(id, map[string]any{})
	}
	if got := sendWithin(t, time.Second, target, "n"); got != Uncertain {
		t.Fatalf("answer without a submission: %s, want uncertain", got)
	}
	target.Attempt = "slow"
	if got := sendWithin(t, 300*time.Millisecond, target, "n"); got != Uncertain {
		t.Fatalf("no answer by the deadline: %s, want uncertain", got)
	}
}
