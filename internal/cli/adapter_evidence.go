package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

const adapterEvidenceUsage = "usage: crosshall owner adapter evidence <file>"

// maxEvidenceFile bounds a conformance run's evidence file.
const maxEvidenceFile = 64 << 10

func runAdapterEvidence(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if len(args) != 3 {
		return o.usage(adapterEvidenceUsage)
	}
	cmd, err := readEvidence(args[2])
	if err != nil {
		return o.usage(err.Error())
	}
	if env.Start != nil {
		if err := env.Start(ctx); err != nil {
			return render(opts, cmd, domain.Fail(core.ExitUnavailable, core.CodeUnsent,
				"dispatcher could not start: "+err.Error(), "crosshall owner start"), stdout, stderr)
		}
	}
	return send(ctx, env, opts, Call{Owner: true, Command: cmd, OpKey: opKey(env, opts)}, stdout, stderr)
}

// readEvidence parses a conformance evidence file; its digest names the run.
func readEvidence(path string) (domain.AdapterEvidence, error) {
	var cmd domain.AdapterEvidence
	file, err := os.Open(path)
	if err != nil {
		return cmd, fmt.Errorf("cannot read the evidence file: %v", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxEvidenceFile+1))
	if err != nil || len(raw) > maxEvidenceFile {
		return cmd, fmt.Errorf("the evidence file is unreadable or larger than %d bytes", maxEvidenceFile)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cmd); err != nil || cmd.ResultRef != "" || decoder.Decode(&struct{}{}) != io.EOF {
		return cmd, fmt.Errorf("the evidence file is not exactly one conformance result")
	}
	cmd.ResultRef = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	return cmd, nil
}

func renderAdapterEvidence(data []byte, w io.Writer) error {
	var r domain.AdapterEvidenceResult
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "Recorded %d result(s) for %s %s on %s, %s mode (manifest %s).\n",
		r.Rows, r.Vendor, r.VendorVersion, r.Platform, r.Mode, r.ManifestRev)
	return err
}
