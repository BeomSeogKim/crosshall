// Package hostclock reads the device's boot incarnation and a monotonic
// clock that keeps counting while the device sleeps (#400, design 6a). A
// monotonic reading is meaningful only within the boot it was taken in.
package hostclock

import "errors"

// errUnsupported is returned where the platform offers neither.
var errUnsupported = errors.New("hostclock: unsupported platform")

// Boot names the device's current boot incarnation: it changes at every
// boot and never otherwise.
func Boot() (string, error) { return boot() }

// Mono is the monotonic time in nanoseconds since an origin fixed for the
// current boot, including time asleep.
func Mono() (int64, error) { return mono() }
