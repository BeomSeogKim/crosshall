// Package link carries hall traffic between a spoke device and the hall's
// home device over a byte stream the user already has, such as the stdio
// of `ssh -T <home> crosshall link serve` (#400). It knows frames and the
// handshake only; it opens no connection and holds no hall state.
package link

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
)

// magic starts every frame, so bytes that are not a frame, such as a login
// banner or a shell rc file's output, are refused instead of read as a
// length.
var magic = [4]byte{'C', 'X', 'H', 'L'}

// MaxFrame bounds one frame's JSON, as the spool bounds one packet.
const MaxFrame = 1 << 20

// ErrStray reports bytes on the stream that are not a frame.
var ErrStray = errors.New("link: unexpected output on the link stream")

// StrayError is ErrStray with the first bytes seen, printable only.
type StrayError struct{ Seen string }

func (e *StrayError) Error() string {
	return fmt.Sprintf("%v: %q (a login banner or shell startup output on the home device? the link needs ssh -T and quiet rc files)", ErrStray, e.Seen)
}

func (e *StrayError) Unwrap() error { return ErrStray }

// Frame is one message: its type and its JSON body.
type Frame struct {
	Type string          `json:"t"`
	Body json.RawMessage `json:"b,omitempty"`
}

// Writer writes frames.
type Writer struct{ w io.Writer }

// NewWriter returns a Writer on w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write encodes body under the frame type t and writes it in one call.
func (w *Writer) Write(t string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err := json.Marshal(Frame{Type: t, Body: raw})
	if err != nil {
		return err
	}
	if len(data) > MaxFrame {
		return fmt.Errorf("link: frame of %d bytes exceeds %d", len(data), MaxFrame)
	}
	buf := make([]byte, 0, 8+len(data))
	buf = append(buf, magic[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(data)))
	buf = append(buf, data...)
	_, err = w.w.Write(buf)
	return err
}

// Reader reads frames.
type Reader struct{ r *bufio.Reader }

// NewReader returns a Reader on r.
func NewReader(r io.Reader) *Reader { return &Reader{r: bufio.NewReader(r)} }

// Read returns the next frame. It returns a *StrayError for bytes that do
// not start a frame, io.EOF at a clean end, and io.ErrUnexpectedEOF for a
// frame cut short.
func (r *Reader) Read() (Frame, error) {
	var head [8]byte
	// The magic is checked byte by byte as it arrives, so a short prefix of
	// something else fails at once instead of waiting for a full header.
	for i := range magic {
		b, err := r.r.ReadByte()
		if err != nil {
			if i == 0 {
				return Frame{}, err
			}
			return Frame{}, io.ErrUnexpectedEOF
		}
		head[i] = b
		if b != magic[i] {
			return Frame{}, r.stray(head[:i+1])
		}
	}
	if _, err := io.ReadFull(r.r, head[4:]); err != nil {
		return Frame{}, io.ErrUnexpectedEOF
	}
	size := binary.BigEndian.Uint32(head[4:])
	if size > MaxFrame {
		return Frame{}, fmt.Errorf("link: frame of %d bytes exceeds %d", size, MaxFrame)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r.r, data); err != nil {
		return Frame{}, io.ErrUnexpectedEOF
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil || f.Type == "" {
		return Frame{}, fmt.Errorf("link: malformed frame")
	}
	return f, nil
}

// stray reads a little more of the line, so the error shows what printed.
func (r *Reader) stray(seen []byte) error {
	more, _ := r.r.Peek(min(r.r.Buffered(), 56))
	b := append(append([]byte{}, seen...), more...)
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	text := []rune{}
	for _, c := range string(b) {
		if unicode.IsPrint(c) {
			text = append(text, c)
		}
	}
	return &StrayError{Seen: string(text)}
}
