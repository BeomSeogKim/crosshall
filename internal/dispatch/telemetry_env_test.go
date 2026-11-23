package dispatch

import (
	"slices"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

func TestDispatcherEnvForwardsEachTelemetryVeto(t *testing.T) {
	for key, value := range map[string]string{telemetryconfig.TelemetryEnv: "off", telemetryconfig.DoNotTrackEnv: "1"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(telemetryconfig.TelemetryEnv, "")
			t.Setenv(telemetryconfig.DoNotTrackEnv, "")
			t.Setenv(key, value)
			got := (LaunchConfig{StateRoot: t.TempDir()}).dispatcherEnv()
			if want := key + "=" + value; !slices.Contains(got, want) {
				t.Fatalf("dispatcher child environment lacks %s", want)
			}
		})
	}
}
