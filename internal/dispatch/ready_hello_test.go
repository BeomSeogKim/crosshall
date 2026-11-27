package dispatch

import (
	"encoding/json"
	"slices"
	"testing"
)

// A dispatcher's ready marker tells a hook it accepts the start intent, a
// prompt's launch proof and a prompt's server, and that its hook.event
// reply names the member's current launch.
func TestTheReadyMarkerOffersWhatTheDispatcherAccepts(t *testing.T) {
	raw, err := json.Marshal(newReadyMarker("instance"))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		Hello []string `json:"hello"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil || !slices.Equal(marker.Hello, []string{"start", "launch_proof", "prompt_host", "launch_ack"}) {
		t.Fatalf("ready marker %s", raw)
	}
}
