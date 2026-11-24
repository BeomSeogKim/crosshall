package dispatch

import (
	"encoding/json"
	"testing"
)

// A dispatcher's ready marker tells a hook it accepts the start intent.
func TestTheReadyMarkerOffersTheStartIntent(t *testing.T) {
	raw, err := json.Marshal(newReadyMarker("instance"))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		Hello []string `json:"hello"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil || len(marker.Hello) != 1 || marker.Hello[0] != "start" {
		t.Fatalf("ready marker %s", raw)
	}
}
