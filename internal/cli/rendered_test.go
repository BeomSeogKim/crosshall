package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

type ackCounter int

func (a *ackCounter) Acknowledge() error {
	*a++
	return nil
}

type truncatedWriter struct{}

func (truncatedWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestRenderAcknowledgesCompleteOutputOnly(t *testing.T) {
	result := core.Result{Data: json.RawMessage(`{"warnings":["new version"]}`)}
	var a ackCounter
	render := func(stdout, _ io.Writer) core.ExitCode {
		io.WriteString(stdout, "members\n")
		return core.ExitOK
	}
	var out bytes.Buffer
	status := RenderAndAcknowledge(&a, result, false, &out, io.Discard, render)
	if status != core.ExitOK || a != 1 || out.String() != "members\nnew version\n" {
		t.Fatalf("plain output: %d, %d, %q", status, a, out.String())
	}
	out.Reset()
	status = RenderAndAcknowledge(&a, result, true, &out, io.Discard, render)
	if status != core.ExitOK || a != 2 || strings.Contains(out.String(), "new version") {
		t.Fatalf("extra JSON output: %d, %d, %q", status, a, out.String())
	}
	status = RenderAndAcknowledge(&a, result, false, truncatedWriter{}, io.Discard, render)
	if status != core.ExitError || a != 2 {
		t.Fatalf("short write advanced cursor: %d, %d", status, a)
	}
}

type countingWriter struct {
	writes, failAt int // failAt 0 never fails
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

// A first command's reply ends with the member rules and the roster changes.
// Whichever write of it fails, the body's or the trailer's, nothing is
// acknowledged: the rules stay owed.
func TestAFailedWriteOfTheRulesTrailerIsNotAcknowledged(t *testing.T) {
	result := core.Result{Data: json.RawMessage(`{"channel":"checkout","members":[{"alias":"claude1","vendor":"claude","self":true}],
		"join_hint":"[crosshall] member rules","roster_changes":[{"alias":"latepeer","vendor":"codex","change":"joined"}],
		"roster_seq":3,"pending":[7],"owed_count":1}`)}
	run := func(w io.Writer) (core.ExitCode, ackCounter) {
		var a ackCounter
		status := RenderAndAcknowledge(&a, result, false, w, io.Discard, func(out, diag io.Writer) core.ExitCode {
			return render(options{}, domain.Who{}, result, out, diag)
		})
		return status, a
	}
	whole := &countingWriter{}
	if status, acked := run(whole); status != core.ExitOK || acked != 1 || whole.writes < 3 {
		t.Fatalf("complete output: status %d, acknowledged %d, %d writes", status, acked, whole.writes)
	}
	for failAt := 1; failAt <= whole.writes; failAt++ {
		if status, acked := run(&countingWriter{failAt: failAt}); status != core.ExitError || acked != 0 {
			t.Errorf("write %d of %d failed: status %d, acknowledged %d", failAt, whole.writes, status, acked)
		}
	}
}
