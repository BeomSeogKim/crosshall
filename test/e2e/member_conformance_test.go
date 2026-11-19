//go:build e2e

package e2e

import (
	"encoding/json"
	"os/exec"
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
	out, err := exec.Command(python, driver, "run", "--crosshall", binary, "--reader", "shell").CombinedOutput()
	if err != nil {
		t.Fatalf("shell scenario: %v\n%s", err, out)
	}
	var receipt struct {
		Identity string            `json:"identity"`
		Checks   map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(out, &receipt); err != nil || receipt.Identity != "route" || len(receipt.Checks) != 6 {
		t.Fatalf("incomplete shell receipt: %v\n%s", err, out)
	}
}
