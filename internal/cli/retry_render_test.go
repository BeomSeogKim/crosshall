package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// A successful retry prints what its original command would have.
func TestARetryRendersAsItsOriginalCommand(t *testing.T) {
	who, _ := json.Marshal(domain.WhoResult{Channel: "checkout"})
	data, _ := json.Marshal(domain.RetryResult{Command: domain.VerbWho, Result: who})
	var out bytes.Buffer
	if err := renderPlain(domain.Retry{Key: "k1"}, data, &out); err != nil || !strings.Contains(out.String(), "#checkout") {
		t.Fatalf("rendered %q, %v", out.String(), err)
	}
}
