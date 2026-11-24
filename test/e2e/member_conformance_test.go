//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedMemberConformance(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("member scenario requires Python 3.8 or later")
	}
	const driver = "../../scripts/protocol-conformance.py"
	if out, err := exec.Command(python, driver, "table", "--check").CombinedOutput(); err != nil {
		t.Fatalf("published table: %v\n%s", err, out)
	}
	// The driver's dispatcher logs to the driver's stderr, and the receipt is
	// stdout alone. A dispatcher that always logs keeps the two apart here.
	if strings.Contains(binary, "'") {
		t.Fatalf("binary path %q cannot be quoted for the wrapper", binary)
	}
	logging := filepath.Join(t.TempDir(), "crosshall")
	wrapper := "#!/bin/sh\nif [ \"$1\" = __dispatch ]; then echo 'dispatcher: a log line' >&2; fi\nexec '" + binary + "' \"$@\"\n"
	if err := os.WriteFile(logging, []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	run := exec.Command(python, driver, "run", "--crosshall", logging, "--reader", "shell")
	run.Stderr = &stderr
	out, err := run.Output()
	if err != nil {
		t.Fatalf("shell scenario: %v\n%s\nstderr:\n%s", err, out, stderr.Bytes())
	}
	if !strings.Contains(stderr.String(), "dispatcher: a log line") {
		t.Fatalf("the dispatcher's log did not reach stderr:\n%s", stderr.Bytes())
	}
	var receipt struct {
		Identity string            `json:"identity"`
		Checks   map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(out, &receipt); err != nil || receipt.Identity != "route" || len(receipt.Checks) != 6 {
		t.Fatalf("incomplete shell receipt: %v\n%s\nstderr:\n%s", err, out, stderr.Bytes())
	}
}
