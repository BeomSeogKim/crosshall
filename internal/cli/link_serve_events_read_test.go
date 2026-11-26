package cli

import (
	"os"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/link"
)

// The home keeps draining a linked device's frames while its loop is busy
// writing a page: a device that writes before it reads finishes its
// writes over a bounded OS pipe though nothing consumes them yet.
func TestTheHomeKeepsReadingWhileItsLoopIsBusy(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	frames := receiveLinkEventFrames(t.Context(), link.NewReader(pr))
	w := link.NewWriter(pw)
	const n = 2000 // far past a pipe buffer
	for i := 1; i <= n; i++ {
		if err := pw.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := w.Write(link.TypeAck, link.Ack{Seq: uint64(i), Boot: "boot"}); err != nil {
			t.Fatalf("the device's write %d waited on the home's loop: %v", i, err)
		}
	}
	for i := 1; i <= n; i++ {
		got := <-frames
		if got.err != nil || got.frame.Type != link.TypeAck {
			t.Fatalf("frame %d: %+v", i, got)
		}
	}
}
