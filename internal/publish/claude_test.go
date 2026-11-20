package publish

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// selfSocket listens on a Unix socket in this process, which the kernel
// then reports as the listener; lines received are sent on the channel.
func selfSocket(t *testing.T) (string, net.Listener, <-chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ch-pub-") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	lines := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if line, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
					lines <- line
				}
			}()
		}
	}()
	return path, ln, lines
}

func selfProbe(path string) Probe {
	return Probe{Target: Target{Adapter: "claude", Address: path}, Route: Route{SocketPath: path},
		LaunchRef: fmt.Sprintf("pid-%d", os.Getpid()), StartedHookAtMS: time.Now().UnixMilli()}
}

func provenSelf(t *testing.T, path string) Target {
	t.Helper()
	queue, ok := proveClaude(t.Context(), selfProbe(path))
	if !ok || queue.PID != int64(os.Getpid()) || queue.StartMS <= 0 {
		t.Fatalf("proof of this process's socket: %+v %t", queue, ok)
	}
	return Target{Adapter: "claude", Address: path, Queue: queue}
}

func TestClaudeSenderWritesOneLaterLineToTheProvenListener(t *testing.T) {
	path, _, lines := selfSocket(t)
	target := provenSelf(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if got := sendClaude(ctx, target, `[crosshall] pending: 3 7. "quoted"`); got != Submitted {
		t.Fatalf("outcome %s", got)
	}
	want := `{"type":"user","message":{"content":"[crosshall] pending: 3 7. \"quoted\""},"from":"crosshall","priority":"later"}` + "\n"
	select {
	case line := <-lines:
		if line != want {
			t.Fatalf("wrote %q, want %q", line, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing written")
	}
}

func TestClaudeProofIsForTheLaunchItNames(t *testing.T) {
	path, _, lines := selfSocket(t)
	probe := selfProbe(path)
	probe.LaunchRef = fmt.Sprintf("pid-%d", os.Getpid()+1)
	if queue, ok := proveClaude(t.Context(), probe); ok {
		t.Fatalf("proved %+v for another launch", queue)
	}
	probe = selfProbe(path)
	probe.StartedHookAtMS = 1 // a launch whose first hook came before this process started
	if _, ok := proveClaude(t.Context(), probe); ok {
		t.Fatal("proved a process that started after its launch's first hook")
	}
	select {
	case line := <-lines:
		t.Fatalf("a proof wrote %q", line)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestClaudeSenderWritesNothingWithoutTheProvenListener(t *testing.T) {
	path, ln, lines := selfSocket(t)
	target := provenSelf(t, path)
	restarted := target
	restarted.Queue.StartMS++ // the PID now names a process that started later
	missing := target
	missing.Address = filepath.Join(filepath.Dir(path), "gone.sock")
	for name, c := range map[string]Target{"another start time": restarted, "no socket": missing} {
		if got := sendClaude(t.Context(), c, "x"); got != NotSubmitted {
			t.Errorf("%s: %s", name, got)
		}
	}
	ln.Close() // the socket file stays; connecting is refused
	if _, err := os.Lstat(path); err == nil {
		if got := sendClaude(t.Context(), target, "x"); got != NotSubmitted {
			t.Errorf("refused: %s", got)
		}
	}
	select {
	case line := <-lines:
		t.Fatalf("wrote %q", line)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestClaudeQueueRetiresOnlyWhenItsProcessIsGone(t *testing.T) {
	path, _, _ := selfSocket(t)
	queue := provenSelf(t, path).Queue
	if claudeRetired(t.Context(), queue) {
		t.Fatal("this running process counts as gone")
	}
	if !claudeRetired(t.Context(), Queue{PID: queue.PID, StartMS: queue.StartMS + 1}) {
		t.Fatal("a PID with another start time is not proof the queue is gone")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no true command:", err)
	}
	if !claudeRetired(t.Context(), Queue{PID: int64(cmd.Process.Pid), StartMS: queue.StartMS}) {
		t.Fatal("an exited, reaped process is not proof the queue is gone")
	}
}
