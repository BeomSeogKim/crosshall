package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// RenderAndAcknowledge records a response cursor only after the chosen
// renderer has successfully written the whole result. JSON renderers receive
// warnings in the result data. Plain renderers print one line per warning.
func RenderAndAcknowledge(transport any, result core.Result, asJSON bool, stdout, stderr io.Writer, render func(io.Writer, io.Writer) core.ExitCode) core.ExitCode {
	out, diag := &checkedWriter{w: stdout}, &checkedWriter{w: stderr}
	status := render(out, diag)
	if status == core.ExitOK && !asJSON {
		var trailer struct {
			Warnings []string `json:"warnings"`
		}
		if json.Unmarshal(result.Data, &trailer) == nil {
			for _, warning := range trailer.Warnings {
				fmt.Fprintln(out, warning)
			}
		}
	}
	if out.err != nil || diag.err != nil {
		return core.ExitError
	}
	if ack, ok := transport.(interface{ Acknowledge() error }); ok && status == core.ExitOK && result.Status == core.ExitOK {
		if err := ack.Acknowledge(); err != nil {
			fmt.Fprintf(stderr, "crosshall: could not acknowledge displayed roster: %v\n", err)
		}
	}
	return status
}

type checkedWriter struct {
	w   io.Writer
	err error
}

func (w *checkedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}
