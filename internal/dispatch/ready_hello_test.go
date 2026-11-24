package dispatch

import (
	"encoding/json"
	"slices"
	"testing"
)

// A dispatcher's ready marker tells a hook it accepts the start intent and
// a prompt's launch proof.
func TestTheReadyMarkerOffersWhatTheDispatcherAccepts(t *testing.T) {
	raw, err := json.Marshal(newReadyMarker("instance"))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		Hello []string `json:"hello"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil || !slices.Equal(marker.Hello, []string{"start", "launch_proof"}) {
		t.Fatalf("ready marker %s", raw)
	}
}
