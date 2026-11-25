// Package appserver is crosshall's client for Codex's managed app server:
// JSON-RPC over a WebSocket on the server's Unix control socket. It is
// written on the standard library's net alone, so the binary links no HTTP
// or TLS stack, and it can send only the methods the notice sender needs.
// Any client on that socket can answer a pending approval; this one never
// answers anything the server asks.
package appserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// Methods the client may send. Everything else is refused before a byte is
// written: turn/start would steer a working turn, and a response to a
// server request could answer an approval.
var allowed = map[string]bool{
	"thread/read":         true,
	"thread/queue/add":    true,
	"thread/queue/list":   true,
	"thread/queue/delete": true,
}

// maxFrame caps one message from the server. A notice's answers are small;
// a thread/read with its turns stays far below it.
const maxFrame = 4 << 20

// ErrNotAllowed is returned for a method outside the allow-list.
var ErrNotAllowed = errors.New("app server method not allowed")

// Identity is the process that must be listening: a PID and its start
// identity (proc.Start).
type Identity struct {
	PID   int64
	Start int64
}

// Client is one connection to the app server.
type Client struct {
	conn net.Conn
	br   *bufio.Reader
	next int64
}

// Error is a JSON-RPC error answer: the server read the call and refused it.
type Error struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("app server error %d: %s", e.Code, e.Message) }

// Dial connects to the control socket at socketPath (a link is resolved
// first), requires the listener to be want, and initializes the session.
// Nothing is written before the listener is checked. The context's
// deadline bounds every later read and write on the connection.
func Dial(ctx context.Context, socketPath string, want Identity) (*Client, error) {
	real, err := filepath.EvalSymlinks(socketPath)
	if err != nil {
		return nil, err
	}
	conn, peer, err := proc.DialSocketPeer(ctx, real)
	if err != nil {
		return nil, err
	}
	if peer.PID != want.PID || peer.Identity != want.Start {
		conn.Close()
		return nil, fmt.Errorf("control socket listener %d is not the proven server %d", peer.PID, want.PID)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, err
		}
	}
	c := &Client{conn: conn, br: bufio.NewReader(conn)}
	if err := c.handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	init := map[string]any{
		"clientInfo":   map[string]string{"name": "crosshall", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}
	if err := c.call("initialize", init, nil); err != nil {
		conn.Close()
		return nil, err
	}
	if err := c.writeJSON(map[string]string{"method": "initialized"}); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// Close ends the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call sends one allowed method and decodes its result into out (which may
// be nil). A JSON-RPC error answer is returned as *Error.
func (c *Client) Call(method string, params, out any) error {
	if !allowed[method] {
		return fmt.Errorf("%w: %s", ErrNotAllowed, method)
	}
	return c.call(method, params, out)
}

func (c *Client) call(method string, params, out any) error {
	c.next++
	id := c.next
	if err := c.writeJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		payload, err := c.readMessage()
		if err != nil {
			return err
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("app server sent malformed JSON: %w", err)
		}
		// A notification, or a request from the server: skipped, never
		// answered.
		if msg.Method != "" || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	}
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func (c *Client) handshake() error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req := "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(c.conn, req); err != nil {
		return err
	}
	status, err := c.readLine()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(status, "HTTP/1.1 101") {
		return fmt.Errorf("app server refused the upgrade: %q", status)
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	wantAccept := base64.StdEncoding.EncodeToString(sum[:])
	accepted := false
	for range 64 {
		line, err := c.readLine()
		if err != nil {
			return err
		}
		if line == "" {
			if !accepted {
				return errors.New("app server upgrade without a matching Sec-WebSocket-Accept")
			}
			return nil
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Sec-WebSocket-Accept") && strings.TrimSpace(value) == wantAccept {
			accepted = true
		}
	}
	return errors.New("app server upgrade header too long")
}

func (c *Client) readLine() (string, error) {
	line, err := c.br.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func (c *Client) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(0x1, data)
}

// writeFrame writes one final, masked frame, as a client must.
func (c *Client) writeFrame(opcode byte, data []byte) error {
	var head bytes.Buffer
	head.WriteByte(0x80 | opcode)
	n := len(data)
	switch {
	case n < 126:
		head.WriteByte(0x80 | byte(n))
	case n <= 0xFFFF:
		head.WriteByte(0x80 | 126)
		_ = binary.Write(&head, binary.BigEndian, uint16(n))
	default:
		head.WriteByte(0x80 | 127)
		_ = binary.Write(&head, binary.BigEndian, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	head.Write(mask[:])
	frame := make([]byte, 0, head.Len()+n)
	frame = append(frame, head.Bytes()...)
	for i, b := range data {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.conn.Write(frame)
	return err
}

// readMessage reads one text message, joining continuation frames and
// answering pings; a close frame or a binary message ends the call.
func (c *Client) readMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return nil, err
		}
		fin, opcode := h[0]&0x80 != 0, h[0]&0x0F
		if h[1]&0x80 != 0 {
			return nil, errors.New("app server sent a masked frame")
		}
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(ext[:])
		}
		if n > maxFrame || uint64(len(msg))+n > maxFrame {
			return nil, errors.New("app server message too large")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		switch opcode {
		case 0x8:
			return nil, errors.New("app server closed the connection")
		case 0x9:
			if err := c.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		case 0x1:
			if started {
				return nil, errors.New("app server started a message inside another")
			}
			started = true
		case 0x0:
			if !started {
				return nil, errors.New("app server sent a continuation without a message")
			}
		default:
			return nil, fmt.Errorf("app server sent frame opcode %d", opcode)
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}
