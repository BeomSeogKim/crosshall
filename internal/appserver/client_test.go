package appserver

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// fakeServer is a WebSocket JSON-RPC listener in this process. It records
// every message a client sends, and answers each call by script.
type fakeServer struct {
	sock string
	ln   net.Listener
	// before is sent ahead of each answer: notifications, server requests.
	before func(id int64, method string) [][]byte
	// answer is the response for a call; nil means {"result":{}}.
	answer func(id int64, method string) []byte

	mu       sync.Mutex
	received []map[string]any
	done     chan struct{}
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "ch-as-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &fakeServer{sock: filepath.Join(dir, "s.sock"), done: make(chan struct{})}
	s.ln, err = net.Listen("unix", s.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.ln.Close(); <-s.done })
	go s.serve()
	return s
}

func (s *fakeServer) serve() {
	defer close(s.done)
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	key := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Sec-WebSocket-Key") {
			key = strings.TrimSpace(value)
		}
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
		base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	for {
		payload, err := readClientFrame(br)
		if err != nil {
			return
		}
		var msg map[string]any
		if json.Unmarshal(payload, &msg) != nil {
			return
		}
		s.mu.Lock()
		s.received = append(s.received, msg)
		s.mu.Unlock()
		idv, hasID := msg["id"].(float64)
		method, _ := msg["method"].(string)
		if !hasID || method == "" {
			continue
		}
		id := int64(idv)
		if s.before != nil {
			for _, m := range s.before(id, method) {
				writeServerFrame(conn, m)
			}
		}
		var out []byte
		if s.answer != nil {
			out = s.answer(id, method)
		}
		if out == nil {
			out, _ = json.Marshal(map[string]any{"id": id, "result": map[string]any{}})
		}
		writeServerFrame(conn, out)
	}
}

func (s *fakeServer) messages() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.received...)
}

func readClientFrame(r io.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	if h[1]&0x80 == 0 {
		return nil, errors.New("client frame not masked")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var e [2]byte
		io.ReadFull(r, e[:])
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		io.ReadFull(r, e[:])
		n = binary.BigEndian.Uint64(e[:])
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return nil, err
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return p, nil
}

func writeServerFrame(w io.Writer, p []byte) {
	head := []byte{0x81}
	switch n := len(p); {
	case n < 126:
		head = append(head, byte(n))
	case n <= 0xFFFF:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	w.Write(append(head, p...))
}

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

func methods(msgs []map[string]any) []string {
	var out []string
	for _, m := range msgs {
		method, _ := m["method"].(string)
		if method == "" {
			method = "<response>"
		}
		out = append(out, method)
	}
	return out
}

func TestCallsAnAllowedMethodAndNeverAnswersTheServer(t *testing.T) {
	s := newFakeServer(t)
	s.before = func(id int64, method string) [][]byte {
		if method != "thread/queue/add" {
			return nil
		}
		return [][]byte{
			[]byte(`{"method":"thread/queue/changed","params":{"threadId":"t1"}}`),
			// An approval request from the server, with an ID that is also
			// the client's call ID: it must be neither answered nor taken
			// for the call's response.
			[]byte(`{"id":` + jsonInt(id) + `,"method":"item/commandExecution/requestApproval","params":{"threadId":"t1"}}`),
			[]byte(`{"id":0,"method":"execCommandApproval","params":{}}`),
		}
	}
	s.answer = func(id int64, method string) []byte {
		if method == "thread/queue/add" {
			return []byte(`{"id":` + jsonInt(id) + `,"result":{"queuedSubmission":{"id":"sub-1","clientUserMessageId":"crosshall-7","input":[]}}}`)
		}
		return nil
	}
	c, err := Dial(ctx(t), s.sock, self(t))
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
	<-s.done
	got := strings.Join(methods(s.messages()), ",")
	if got != "initialize,initialized,thread/queue/add" {
		t.Fatalf("client sent %s; want only initialize, initialized and the call", got)
	}
}

func TestRefusesEveryMethodOutsideTheAllowList(t *testing.T) {
	s := newFakeServer(t)
	c, err := Dial(ctx(t), s.sock, self(t))
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
	<-s.done
	if got := strings.Join(methods(s.messages()), ","); got != "initialize,initialized" {
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
	s := newFakeServer(t)
	s.answer = func(id int64, method string) []byte {
		if method == "thread/read" {
			return []byte(`{"id":` + jsonInt(id) + `,"error":{"code":-32600,"message":"thread not found"}}`)
		}
		return nil
	}
	c, err := Dial(ctx(t), s.sock, self(t))
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
	s := newFakeServer(t)
	me := self(t)
	for _, want := range []Identity{{PID: me.PID, Start: me.Start + 1}, {PID: me.PID + 1, Start: me.Start}} {
		if _, err := Dial(ctx(t), s.sock, want); err == nil {
			t.Fatalf("Dial accepted a listener for %+v", want)
		}
	}
	s.ln.Close()
	<-s.done
	if got := s.messages(); len(got) != 0 {
		t.Fatalf("client wrote %v to an unproven listener", methods(got))
	}
}

func TestFollowsTheControlSocketLink(t *testing.T) {
	s := newFakeServer(t)
	link := filepath.Join(filepath.Dir(s.sock), "app-server-control.sock")
	if err := os.Symlink(s.sock, link); err != nil {
		t.Fatal(err)
	}
	c, err := Dial(ctx(t), link, self(t))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestRefusesAnOversizedMessage(t *testing.T) {
	s := newFakeServer(t)
	s.answer = func(id int64, method string) []byte {
		if method == "thread/queue/list" {
			return []byte(`{"id":` + jsonInt(id) + `,"result":{"data":[],"pad":"` + strings.Repeat("x", maxFrame) + `"}}`)
		}
		return nil
	}
	c, err := Dial(ctx(t), s.sock, self(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call("thread/queue/list", map[string]any{"threadId": "t"}, nil); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
