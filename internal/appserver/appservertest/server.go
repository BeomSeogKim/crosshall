// Package appservertest is a stand-in Codex app server for tests: a
// WebSocket JSON-RPC listener on a Unix socket in the test's own process,
// which records every message a client sends and answers by script.
package appservertest

import (
	"bufio"
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
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Server is one stand-in app server. Set Before and Answer before a client
// connects.
type Server struct {
	// Socket is the listening path; Dir holds it.
	Socket, Dir string
	// Before is sent ahead of each answer: notifications, server requests.
	Before func(id int64, method string, params map[string]any) [][]byte
	// Answer is the response for a call; nil means {"result":{}}.
	Answer func(id int64, method string, params map[string]any) []byte

	ln       net.Listener
	mu       sync.Mutex
	received []map[string]any
	conns    sync.WaitGroup
}

// Start listens on a fresh socket and serves clients until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("", "ch-as-")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Dir: dir, Socket: filepath.Join(dir, "s.sock")}
	if s.ln, err = net.Listen("unix", s.Socket); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := s.ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go func() {
				defer s.conns.Done()
				s.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		s.ln.Close()
		<-accepted
		s.conns.Wait()
		os.RemoveAll(dir)
	})
	return s
}

// Stop closes the listener; connections already open run on.
func (s *Server) Stop() { s.ln.Close() }

// Received lists the messages clients sent, in order.
func (s *Server) Received() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.received...)
}

// Methods lists the method of each received message; a message without
// one (a response to a server request) is "<response>".
func (s *Server) Methods() []string {
	var out []string
	for _, m := range s.Received() {
		method, _ := m["method"].(string)
		if method == "" {
			method = "<response>"
		}
		out = append(out, method)
	}
	return out
}

func (s *Server) serve(conn net.Conn) {
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
		params, _ := msg["params"].(map[string]any)
		if s.Before != nil {
			for _, m := range s.Before(id, method, params) {
				WriteFrame(conn, m)
			}
		}
		var out []byte
		if s.Answer != nil {
			out = s.Answer(id, method, params)
		}
		if out == nil {
			out = Result(id, map[string]any{})
		}
		WriteFrame(conn, out)
	}
}

// Result is a JSON-RPC success answer.
func Result(id int64, result any) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "result": result})
	return b
}

// Error is a JSON-RPC error answer.
func Error(id, code int64, message string) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
	return b
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
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return nil, err
		}
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return nil, err
		}
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

// WriteFrame writes one unmasked text frame, as a server does.
func WriteFrame(w io.Writer, p []byte) {
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
