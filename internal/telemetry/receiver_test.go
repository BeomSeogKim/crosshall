package telemetry

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

// A release build sends to the shipping receiver; a development or test
// build has none, so it neither asks the question nor sends.
func TestReceiver(t *testing.T) {
	const shipping = "https://telemetry.addylog.dev/v1/report"
	if telemetryconfig.Endpoint != shipping {
		t.Fatalf("endpoint %q, want %q", telemetryconfig.Endpoint, shipping)
	}
	for _, version := range []string{"v0.2.0", "v1.2.3", "v0.2.0-rc.3"} {
		if got := Receiver(version); got != shipping {
			t.Errorf("Receiver(%q) = %q", version, got)
		}
	}
	for _, version := range []string{"dev", "", "v0.0.0-readme-test", "v1.2.3-dirty", "(devel)", "1.2.3"} {
		if got := Receiver(version); got != "" {
			t.Errorf("Receiver(%q) = %q, want none", version, got)
		}
	}
}
