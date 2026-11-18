package dispatch

import (
	"fmt"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

const clockOffsetEnv = "CROSSHALL_TEST_CLOCK_OFFSET"

func allowsTestClock(version string) bool {
	return version == "dev" || version == "test"
}

// DomainClock reads the development clock once when the dispatcher starts.
// Transport deadlines always use wall time.
func DomainClock(version string) (core.Clock, error) {
	if !allowsTestClock(version) {
		return time.Now, nil
	}
	raw := os.Getenv(clockOffsetEnv)
	if raw == "" {
		return time.Now, nil
	}
	offset, err := time.ParseDuration(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be a signed duration such as 2m", clockOffsetEnv)
	}
	return func() time.Time { return time.Now().Add(offset) }, nil
}
