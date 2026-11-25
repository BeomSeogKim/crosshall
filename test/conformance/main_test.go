//go:build conformance

package conformance

import (
	"os"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/testreap"
)

// Fail the run when a test leaves a fixture dispatcher behind.
func TestMain(m *testing.M) { os.Exit(testreap.Check(m.Run())) }
