package link

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A frame to a link whose reader never reads is bounded by its deadline,
// and says whether any byte reached the pipe: a partial frame did leave,
// a frame into a full pipe did not.
func TestAFrameIsBoundedByItsDeadline(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	count := &countingWriter{w: write}
	w := NewWriter(count)
	started := time.Now()
	sent, err := sendFrame(write, count, w, "call", map[string]string{"body": strings.Repeat("x", 512<<10)}, time.Now().Add(50*time.Millisecond))
	if !sent || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a frame larger than the pipe: sent %v, %v", sent, err)
	}
	sent, err = sendFrame(write, count, w, "call", map[string]string{}, time.Now().Add(30*time.Millisecond))
	if sent || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a frame into a full pipe: sent %v, %v", sent, err)
	}
	if _, err := receiveFrame(read, NewReader(strings.NewReader("")), time.Now()); err == nil {
		t.Fatal("an empty stream produced a frame")
	}
	idle, idleWrite, _ := os.Pipe()
	defer idle.Close()
	defer idleWrite.Close()
	if _, err := receiveFrame(idle, NewReader(idle), time.Now().Add(30*time.Millisecond)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a silent home: %v", err)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("bounded frames took %v", waited)
	}
}
