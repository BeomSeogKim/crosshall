//go:build conformance

package conformance

import (
	"fmt"
	"os"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/testreap"
)

// Fail the run when a test leaves a fixture dispatcher behind.
func TestMain(m *testing.M) {
	if os.Getenv(nativeFixtureWatchEnv) == "1" {
		if err := watchNativeFixture(); err != nil {
			fmt.Fprintln(os.Stderr, "native fixture watchdog:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testreap.Check(m.Run()))
}
