package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
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
