//go:build testvendorhome

package dispatch

import (
	"fmt"
	"os"
	"strconv"
)

// linkInterruptEnv stops crosshall link after a numbered step of replacing
// a stopped profile, so tests can check that every step is restartable.
const linkInterruptEnv = "CROSSHALL_TEST_LINK_INTERRUPT"

func linkInterrupt(step int) error {
	if os.Getenv(linkInterruptEnv) == strconv.Itoa(step) {
		return fmt.Errorf("interrupted after step %d", step)
	}
	return nil
}
