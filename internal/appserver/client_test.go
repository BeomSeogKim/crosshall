package appserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/appserver/appservertest"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

func self(t *testing.T) Identity {
	t.Helper()
	start, err := proc.StartOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return Identity{PID: int64(os.Getpid()), Start: start.Identity}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestCallsAnAllowedMethodAndNeverAnswersTheServer(t *testing.T) {
	s := appservertest.Start(t)
	s.Before = func(id int64, method string, _ map[string]any) [][]byte {
		if method != "thread/queue/add" {
			return nil
		}
		return [][]byte{
			[]byte(`{"method":"thread/queue/changed","params":{"threadId":"t1"}}`),
			// An approval request from the server, with an ID that is also
			// the client's call ID: it must be neither answered nor taken
			// for the call's response.
			[]byte(fmt.Sprintf(`{"id":%d,"method":"item/commandExecution/requestApproval","params":{"threadId":"t1"}}`, id)),
			[]byte(`{"id":0,"method":"execCommandApproval","params":{}}`),
		}
	}
	s.Answer = func(id int64, method string, _ map[string]any) []byte {
		if method == "thread/queue/add" {
			return appservertest.Result(id, map[string]any{"queuedSubmission": map[string]any{"id": "sub-1", "clientUserMessageId": "crosshall-7", "input": []any{}}})
		}
		return nil
	}
	c, err := Dial(ctx(t), s.Socket, self(t))
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		QueuedSubmission struct{ ID string } `json:"queuedSubmission"`
	}
	if err := c.Call("thread/queue/add", map[string]any{"threadId": "t1"}, &res); err != nil {
		t.Fatal(err)
	}
	if res.QueuedSubmission.ID != "sub-1" {
		t.Fatalf("submission = %q", res.QueuedSubmission.ID)
	}
	c.Close()
	got := waitMethods(s, 3)
	if got != "initialize,initialized,thread/queue/add" {
		t.Fatalf("client sent %s; want only initialize, initialized and the call", got)
	}
}

func TestRefusesEveryMethodOutsideTheAllowList(t *testing.T) {
	s := appservertest.Start(t)
	c, err := Dial(ctx(t), s.Socket, self(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"turn/start", "turn/steer", "turn/interrupt", "thread/resume", "thread/start",
		"initialize", "item/commandExecution/requestApproval", "command/exec", ""} {
		if err := c.Call(m, map[string]any{}, nil); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("Call(%q) = %v, want ErrNotAllowed", m, err)
		}
	}
	c.Close()
	if got := waitMethods(s, 2); got != "initialize,initialized" {
		t.Fatalf("client sent %s", got)
	}
}

func TestAllowListIsTheNoticeSendersMethods(t *testing.T) {
	want := []string{"thread/queue/add", "thread/queue/delete", "thread/queue/list", "thread/read"}
	if len(allowed) != len(want) {
		t.Fatalf("allow-list has %d methods, want %v", len(allowed), want)
	}
	for _, m := range want {
		if !allowed[m] {
			t.Fatalf("%s missing from the allow-list", m)
		}
	}
}

func TestReturnsTheServersErrorAnswer(t *testing.T) {
	s := appservertest.Start(t)
	s.Answer = func(id int64, method string, _ map[string]any) []byte {
		if method == "thread/read" {
			return appservertest.Error(id, -32600, "thread not found")
		}
		return nil
	}
	c, err := Dial(ctx(t), s.Socket, self(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var rpcErr *Error
	if err := c.Call("thread/read", map[string]any{"threadId": "x"}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32600 {
		t.Fatalf("err = %v", err)
	}
}

func TestWritesNothingToAListenerThatIsNotTheProvenServer(t *testing.T) {
	s := appservertest.Start(t)
	me := self(t)
	for _, want := range []Identity{{PID: me.PID, Start: me.Start + 1}, {PID: me.PID + 1, Start: me.Start}} {
		if _, err := Dial(ctx(t), s.Socket, want); err == nil {
			t.Fatalf("Dial accepted a listener for %+v", want)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := s.Methods(); len(got) != 0 {
		t.Fatalf("client wrote %v to an unproven listener", got)
	}
}

func TestFollowsTheControlSocketLink(t *testing.T) {
	s := appservertest.Start(t)
	link := filepath.Join(filepath.Dir(s.Socket), "app-server-control.sock")
	if err := os.Symlink(s.Socket, link); err != nil {
		t.Fatal(err)
	}
	c, err := Dial(ctx(t), link, self(t))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestRefusesAnOversizedMessage(t *testing.T) {
	s := appservertest.Start(t)
	s.Answer = func(id int64, method string, _ map[string]any) []byte {
		if method == "thread/queue/list" {
			return appservertest.Result(id, map[string]any{"data": []any{}, "pad": strings.Repeat("x", maxFrame)})
		}
		return nil
	}
	c, err := Dial(ctx(t), s.Socket, self(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call("thread/queue/list", map[string]any{"threadId": "t"}, nil); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
}

// waitMethods waits until the server has n messages, then lists their methods.
func waitMethods(s *appservertest.Server, n int) string {
	for range 100 {
		if len(s.Received()) >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	return strings.Join(s.Methods(), ",")
}
